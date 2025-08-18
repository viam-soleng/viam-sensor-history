package sensorreplay

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/pkg/errors"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/utils/rpc"

	// Using the app package for client creation
	"go.viam.com/rdk/app"
)

// Model defines our sensor-replay component model
var Model = resource.NewModel("hunter", "sensor-replay", "sensor")

// replayDataPoint stores a single historical reading with its timestamp
type replayDataPoint struct {
	timestamp time.Time
	readings  map[string]interface{}
}

// Config holds the configuration attributes for the replay sensor
type Config struct {
	SourceComponentName string  `json:"source_component_name"`
	SourceComponentType string  `json:"source_component_type"`
	OrganizationID      string  `json:"organization_id"`
	StartTimeUTC        string  `json:"start_time_utc"`
	EndTimeUTC          string  `json:"end_time_utc"`
	APIKeyID            string  `json:"api_key_id"`
	APIKey              string  `json:"api_key"`
	Loop                bool    `json:"loop"`
	SpeedMultiplier     float64 `json:"speed_multiplier,omitempty"` // Optional: replay at different speeds
	CacheSize           int     `json:"cache_size,omitempty"`       // Optional: limit memory usage
}

// Validate ensures all required configuration fields are present
func (cfg *Config) Validate(path string) ([]string, error) {
	if cfg.SourceComponentName == "" {
		return nil, errors.New("source_component_name is required")
	}
	if cfg.SourceComponentType == "" {
		return nil, errors.New("source_component_type is required")
	}
	if cfg.OrganizationID == "" {
		return nil, errors.New("organization_id is required")
	}
	if cfg.StartTimeUTC == "" {
		return nil, errors.New("start_time_utc is required")
	}
	if cfg.EndTimeUTC == "" {
		return nil, errors.New("end_time_utc is required")
	}
	if cfg.APIKeyID == "" {
		return nil, errors.New("api_key_id is required")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("api_key is required")
	}

	// Set defaults for optional fields
	if cfg.SpeedMultiplier <= 0 {
		cfg.SpeedMultiplier = 1.0
	}
	if cfg.CacheSize <= 0 {
		cfg.CacheSize = 10000 // Default to 10k data points max
	}

	return nil, nil
}

// replaySensor is the main struct for our component
type replaySensor struct {
	resource.Named
	mu              sync.RWMutex
	logger          logging.Logger
	cfg             *Config
	startTime       time.Time
	endTime         time.Time
	replayData      []replayDataPoint
	replayStartTime time.Time
	isReady         bool
	currentIndex    int
	loopCount       int // Track number of loops completed
	cancelCtx       context.Context
	cancelFunc      context.CancelFunc

	// Statistics
	stats struct {
		totalDataPoints int
		fetchDuration   time.Duration
		lastReadingTime time.Time
		loopsCompleted  int
	}
}

func init() {
	registration := resource.Registration[sensor.Sensor, *Config]{
		Constructor: newReplaySensor,
	}
	resource.RegisterComponent(sensor.API, Model, registration)
}

// newReplaySensor is the constructor for the replay sensor
func newReplaySensor(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (sensor.Sensor, error) {
	rs := &replaySensor{
		Named:  conf.ResourceName().AsNamed(),
		logger: logger,
	}

	if err := rs.Reconfigure(ctx, deps, conf); err != nil {
		return nil, err
	}
	return rs, nil
}

// Reconfigure handles the initial setup and subsequent configuration updates
func (rs *replaySensor) Reconfigure(ctx context.Context, deps resource.Dependencies, conf resource.Config) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	// Cancel any previous background tasks
	if rs.cancelFunc != nil {
		rs.cancelFunc()
	}
	rs.cancelCtx, rs.cancelFunc = context.WithCancel(context.Background())
	rs.isReady = false
	rs.loopCount = 0

	newConfig, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return err
	}
	rs.cfg = newConfig

	// Parse timestamps from config
	rs.startTime, err = time.Parse(time.RFC3339, newConfig.StartTimeUTC)
	if err != nil {
		return errors.Wrap(err, "failed to parse start_time_utc")
	}
	rs.endTime, err = time.Parse(time.RFC3339, newConfig.EndTimeUTC)
	if err != nil {
		return errors.Wrap(err, "failed to parse end_time_utc")
	}

	// Validate time range
	if rs.endTime.Before(rs.startTime) {
		return errors.New("end_time_utc must be after start_time_utc")
	}

	rs.logger.Infof("Configuring sensor-replay for component '%s' from %s to %s",
		rs.cfg.SourceComponentName, rs.startTime.Format(time.RFC3339), rs.endTime.Format(time.RFC3339))

	// Fetch data in the background to not block startup
	go rs.fetchAndPrepareData(rs.cancelCtx)

	return nil
}

// fetchAndPrepareData connects to the Viam app, downloads, and prepares the data for replay
func (rs *replaySensor) fetchAndPrepareData(ctx context.Context) {
	fetchStart := time.Now()
	rs.logger.Info("Starting data fetch for replay sensor...")

	// Establish connection to Viam App
	creds := rpc.Credentials{
		Type:    rpc.CredentialsTypeAPIKey,
		Payload: rs.cfg.APIKey,
	}

	dialOpts := []rpc.DialOption{
		rpc.WithEntity(rs.cfg.APIKeyID),
		rpc.WithCredentials(creds),
	}

	client, err := app.NewViamClient(ctx, app.ViamClientConfig{
		BaseURL:     "https://app.viam.com",
		DialOptions: &dialOpts,
	})
	if err != nil {
		rs.logger.Errorf("Failed to connect to Viam app: %v", err)
		return
	}
	defer func() {
		if err := client.Close(); err != nil {
			rs.logger.Warnf("Error closing client: %v", err)
		}
	}()

	// Create data client and prepare filter
	dataClient := client.DataClient()

	// Build the filter for TabularDataByFilter
	filter := app.Filter{
		ComponentName: rs.cfg.SourceComponentName,
		ComponentType: rs.cfg.SourceComponentType,
		Interval: &app.CaptureInterval{
			Start: rs.startTime,
			End:   rs.endTime,
		},
		OrganizationIds: []string{rs.cfg.OrganizationID},
	}

	var allData []replayDataPoint
	limit := 1000 // Fetch 1000 records at a time
	var last string

	for {
		if ctx.Err() != nil {
			rs.logger.Info("Data fetch cancelled")
			return
		}

		rs.logger.Debugf("Fetching data page (last: %s)...", last)

		// Fetch a page of data
		dataPoints, lastID, err := dataClient.TabularDataByFilter(
			ctx,
			filter,
			limit,
			last,
			"",   // sortOrder - empty for default
			true, // includeBinary
		)

		if err != nil {
			rs.logger.Errorf("Failed to fetch tabular data: %v", err)
			return
		}

		// Process the data points
		for _, dp := range dataPoints {
			if dp.Data != nil {
				readings, ok := dp.Data["readings"].(map[string]interface{})
				if ok && dp.TimeReceived != nil {
					allData = append(allData, replayDataPoint{
						timestamp: *dp.TimeReceived,
						readings:  readings,
					})
				}
			}
		}

		rs.logger.Debugf("Fetched %d data points in this page", len(dataPoints))

		// Check if we've fetched all data or hit cache limit
		if lastID == "" || len(dataPoints) == 0 || len(allData) >= rs.cfg.CacheSize {
			break
		}
		last = lastID
	}

	if len(allData) == 0 {
		rs.logger.Warn("No data found for the specified time range. Readings will be empty.")
		return
	}

	// Sort data chronologically
	sort.Slice(allData, func(i, j int) bool {
		return allData[i].timestamp.Before(allData[j].timestamp)
	})

	// Trim to cache size if needed
	if len(allData) > rs.cfg.CacheSize {
		rs.logger.Warnf("Trimming data from %d to cache size limit of %d points",
			len(allData), rs.cfg.CacheSize)
		allData = allData[:rs.cfg.CacheSize]
	}

	fetchDuration := time.Since(fetchStart)
	rs.logger.Infof("Successfully fetched and sorted %d data points in %v",
		len(allData), fetchDuration)

	// Lock to safely update the shared state
	rs.mu.Lock()
	defer rs.mu.Unlock()

	rs.replayData = allData
	rs.currentIndex = 0
	rs.replayStartTime = time.Now()
	rs.isReady = true

	// Update statistics
	rs.stats.totalDataPoints = len(allData)
	rs.stats.fetchDuration = fetchDuration

	// Log time range of data
	if len(allData) > 0 {
		firstTime := allData[0].timestamp
		lastTime := allData[len(allData)-1].timestamp
		duration := lastTime.Sub(firstTime)
		rs.logger.Infof("Data spans from %s to %s (duration: %v)",
			firstTime.Format(time.RFC3339),
			lastTime.Format(time.RFC3339),
			duration)
	}
}

// Readings returns sensor data based on the replay timeline
func (rs *replaySensor) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	if !rs.isReady || len(rs.replayData) == 0 {
		return map[string]interface{}{
			"status": "loading",
			"ready":  rs.isReady,
		}, nil
	}

	// Calculate elapsed time with speed multiplier
	elapsed := time.Since(rs.replayStartTime)
	if rs.cfg.SpeedMultiplier != 1.0 {
		elapsed = time.Duration(float64(elapsed) * rs.cfg.SpeedMultiplier)
	}

	// Handle looping
	if rs.cfg.Loop && len(rs.replayData) > 1 {
		firstTimestamp := rs.replayData[0].timestamp
		lastTimestamp := rs.replayData[len(rs.replayData)-1].timestamp
		totalDuration := lastTimestamp.Sub(firstTimestamp)

		if elapsed > totalDuration {
			// Calculate how many complete loops have passed
			loops := int(elapsed / totalDuration)
			elapsed = elapsed % totalDuration

			// Update state if we've completed new loops
			if loops > rs.loopCount {
				rs.mu.RUnlock()
				rs.mu.Lock()
				rs.loopCount = loops
				rs.stats.loopsCompleted = loops
				rs.currentIndex = 0
				rs.logger.Infof("Completed loop %d, restarting replay", loops)
				rs.mu.Unlock()
				rs.mu.RLock()
			}
		}
	}

	// Calculate the current position in historical time
	currentHistoricalTime := rs.replayData[0].timestamp.Add(elapsed)

	// Find the appropriate reading for the current time
	var foundReading map[string]interface{}
	foundIndex := rs.currentIndex

	for i := rs.currentIndex; i < len(rs.replayData); i++ {
		dp := rs.replayData[i]
		if dp.timestamp.After(currentHistoricalTime) {
			break
		}
		foundReading = dp.readings
		foundIndex = i
	}

	// Update current index if changed
	if foundIndex != rs.currentIndex {
		rs.mu.RUnlock()
		rs.mu.Lock()
		rs.currentIndex = foundIndex
		rs.stats.lastReadingTime = time.Now()
		rs.mu.Unlock()
		rs.mu.RLock()
	}

	if foundReading == nil {
		// Before first data point
		return map[string]interface{}{
			"status":       "waiting_for_data",
			"next_data_in": rs.replayData[0].timestamp.Sub(currentHistoricalTime).Seconds(),
		}, nil
	}

	// Add metadata to the reading
	result := make(map[string]interface{})
	for k, v := range foundReading {
		result[k] = v
	}

	// Add replay metadata if requested
	if extra != nil && extra["include_metadata"] == true {
		result["_replay_metadata"] = map[string]interface{}{
			"historical_timestamp": rs.replayData[foundIndex].timestamp.Format(time.RFC3339),
			"replay_position":      fmt.Sprintf("%d/%d", foundIndex+1, len(rs.replayData)),
			"loop_count":           rs.loopCount,
			"speed_multiplier":     rs.cfg.SpeedMultiplier,
		}
	}

	return result, nil
}

// DoCommand implements custom commands for the sensor
func (rs *replaySensor) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	// Get statistics command
	if _, ok := cmd["get_stats"]; ok {
		rs.mu.RLock()
		defer rs.mu.RUnlock()

		return map[string]interface{}{
			"total_data_points": rs.stats.totalDataPoints,
			"fetch_duration_ms": rs.stats.fetchDuration.Milliseconds(),
			"is_ready":          rs.isReady,
			"current_index":     rs.currentIndex,
			"loops_completed":   rs.stats.loopsCompleted,
			"last_reading_time": rs.stats.lastReadingTime.Format(time.RFC3339),
		}, nil
	}

	// Reset replay command
	if _, ok := cmd["reset"]; ok {
		rs.mu.Lock()
		defer rs.mu.Unlock()

		rs.currentIndex = 0
		rs.loopCount = 0
		rs.replayStartTime = time.Now()
		rs.logger.Info("Replay reset to beginning")

		return map[string]interface{}{"status": "reset"}, nil
	}

	// Jump to specific time command
	if jumpTo, ok := cmd["jump_to_percent"]; ok {
		percent, ok := jumpTo.(float64)
		if !ok {
			return nil, errors.New("jump_to_percent must be a number between 0 and 100")
		}

		if percent < 0 || percent > 100 {
			return nil, errors.New("jump_to_percent must be between 0 and 100")
		}

		rs.mu.Lock()
		defer rs.mu.Unlock()

		if len(rs.replayData) > 0 {
			targetIndex := int(float64(len(rs.replayData)-1) * (percent / 100.0))
			rs.currentIndex = targetIndex

			// Adjust replay start time to match the jump
			firstTime := rs.replayData[0].timestamp
			targetTime := rs.replayData[targetIndex].timestamp
			timeOffset := targetTime.Sub(firstTime)
			rs.replayStartTime = time.Now().Add(-timeOffset)

			rs.logger.Infof("Jumped to %d%% (index %d)", int(percent), targetIndex)

			return map[string]interface{}{
				"status":    "jumped",
				"index":     targetIndex,
				"timestamp": targetTime.Format(time.RFC3339),
			}, nil
		}
	}

	return nil, errors.New("unknown command")
}

// Close gracefully shuts down the sensor
func (rs *replaySensor) Close(ctx context.Context) error {
	rs.logger.Info("Closing replay sensor")

	rs.mu.Lock()
	defer rs.mu.Unlock()

	if rs.cancelFunc != nil {
		rs.cancelFunc()
		rs.cancelFunc = nil
	}

	// Log final statistics
	if rs.stats.totalDataPoints > 0 {
		rs.logger.Infof("Replay sensor closed. Processed %d data points, completed %d loops",
			rs.stats.totalDataPoints, rs.stats.loopsCompleted)
	}

	return nil
}
