package sensorhistory

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"go.viam.com/rdk/app"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

// Model defines our sensor-history component model
var Model = resource.NewModel("hunter", "sensor-history", "sensor")

// historyDataPoint stores a single historical reading with its timestamp
type historyDataPoint struct {
	timestamp time.Time
	readings  map[string]interface{}
}

// Config holds the configuration attributes for the history sensor
type Config struct {
	SourceComponentName string  `json:"source_component_name"`
	SourceComponentType string  `json:"source_component_type"`
	StartTimeUTC        string  `json:"start_time_utc"`
	EndTimeUTC          string  `json:"end_time_utc"`
	Loop                bool    `json:"loop"`
	SpeedMultiplier     float64 `json:"speed_multiplier"`
	CacheSize           int     `json:"cache_size,omitempty"`
	IncludeMetadata     bool    `json:"include_metadata,omitempty"`
}

// Validate ensures all required configuration fields are present
func (cfg *Config) Validate(path string) (implicit []string, explicit []string, err error) {
	if cfg.SourceComponentName == "" {
		return nil, nil, errors.New("source_component_name is required")
	}
	if cfg.SourceComponentType == "" {
		return nil, nil, errors.New("source_component_type is required")
	}
	if cfg.StartTimeUTC == "" {
		return nil, nil, errors.New("start_time_utc is required")
	}
	if cfg.EndTimeUTC == "" {
		return nil, nil, errors.New("end_time_utc is required")
	}
	if cfg.SpeedMultiplier <= 0 {
		return nil, nil, errors.New("speed_multiplier is required and must be positive")
	}

	// Defaults
	if cfg.CacheSize <= 0 {
		cfg.CacheSize = 10000
	}

	return nil, nil, nil
}

// historySensor is the main struct for our component
type historySensor struct {
	resource.Named
	mu               sync.RWMutex
	logger           logging.Logger
	cfg              *Config
	startTime        time.Time
	endTime          time.Time
	historyData      []historyDataPoint
	historyStartTime time.Time
	isReady          bool
	currentIndex     int
	loopCount        int
	cancelCtx        context.Context
	cancelFunc       context.CancelFunc

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
		Constructor: newHistorySensor,
	}
	resource.RegisterComponent(sensor.API, Model, registration)
}

// newHistorySensor is the constructor for the history sensor
func newHistorySensor(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (sensor.Sensor, error) {
	rs := &historySensor{
		Named:  conf.ResourceName().AsNamed(),
		logger: logger,
	}

	if err := rs.Reconfigure(ctx, deps, conf); err != nil {
		return nil, err
	}
	return rs, nil
}

// NewSensor creates a new history sensor (exported constructor for CLI usage)
func NewSensor(ctx context.Context, deps resource.Dependencies, name resource.Name, cfg *Config, logger logging.Logger) (sensor.Sensor, error) {
	ps := &historySensor{
		Named:  name.AsNamed(),
		logger: logger,
	}

	// Create a proper config for Reconfigure
	conf := resource.Config{
		Name:                name.Name,
		API:                 sensor.API,
		Model:               Model,
		ConvertedAttributes: cfg,
	}

	if err := ps.Reconfigure(ctx, deps, conf); err != nil {
		return nil, err
	}
	return ps, nil
}

// Reconfigure handles the initial setup and subsequent configuration updates
func (rs *historySensor) Reconfigure(ctx context.Context, deps resource.Dependencies, conf resource.Config) error {
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

	rs.logger.Infof("Configuring sensor-history for component '%s' from %s to %s (speed: %.1fx, metadata: %t)",
		rs.cfg.SourceComponentName,
		rs.startTime.Format(time.RFC3339),
		rs.endTime.Format(time.RFC3339),
		rs.cfg.SpeedMultiplier,
		rs.cfg.IncludeMetadata)

	// Fetch data in the background to not block startup
	go rs.fetchAndPrepareData(rs.cancelCtx)

	return nil
}

// mustEnv returns the env var value or a descriptive error if missing/empty
func mustEnv(name string) (string, error) {
	v, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(v) == "" {
		return "", errors.Errorf("%s is not set; this module reads credentials from the Viam module environment", name)
	}
	return v, nil
}

// resolveAPICredentials reads API credentials strictly from environment
func (rs *historySensor) resolveAPICredentials() (string, string, error) {
	apiKeyID, err := mustEnv("VIAM_API_KEY_ID")
	if err != nil {
		return "", "", err
	}
	apiKey, err := mustEnv("VIAM_API_KEY")
	if err != nil {
		return "", "", err
	}

	// Indicate presence only
	rs.logger.Debug("Using VIAM_API_KEY_ID and VIAM_API_KEY from module environment (values hidden)")
	return apiKeyID, apiKey, nil
}

// resolveOrganizationID uses VIAM_PRIMARY_ORG_ID from module environment
func (rs *historySensor) resolveOrganizationID() (string, error) {
	org, ok := os.LookupEnv("VIAM_PRIMARY_ORG_ID")
	if !ok || strings.TrimSpace(org) == "" {
		return "", errors.New("VIAM_PRIMARY_ORG_ID is not set in module environment")
	}
	rs.logger.Debug("Using VIAM_PRIMARY_ORG_ID from module environment")
	return org, nil
}

// fetchAndPrepareData connects to the Viam app, downloads, and prepares the data history
func (rs *historySensor) fetchAndPrepareData(ctx context.Context) {
	fetchStart := time.Now()
	rs.logger.Info("Starting data fetch for history sensor...")

	// Resolve creds and org strictly from environment / safe defaults
	apiKeyID, apiKey, err := rs.resolveAPICredentials()
	if err != nil {
		rs.logger.Errorf("Failed to resolve API credentials: %v", err)
		return
	}
	orgID, err := rs.resolveOrganizationID()
	if err != nil {
		rs.logger.Errorf("Failed to resolve organization_id: %v", err)
		return
	}

	// Create Viam client using API key authentication
	opts := app.Options{
		BaseURL: "https://app.viam.com",
	}

	client, err := app.CreateViamClientWithAPIKey(
		ctx,
		opts,
		apiKey,
		apiKeyID,
		rs.logger,
	)
	if err != nil {
		rs.logger.Errorf("Failed to connect to Viam app: %v", err)
		return
	}
	defer func() {
		if err := client.Close(); err != nil {
			rs.logger.Warnf("Error closing client: %v", err)
		}
	}()

	// Create data client
	dataClient := client.DataClient()

	// Build the filter for TabularDataByFilter
	filterOpts := &app.DataByFilterOptions{
		Filter: &app.Filter{
			ComponentName: rs.cfg.SourceComponentName,
			ComponentType: rs.cfg.SourceComponentType,
			Interval: app.CaptureInterval{
				Start: rs.startTime,
				End:   rs.endTime,
			},
			OrganizationIDs: []string{orgID},
		},
		Limit: 1000,
	}

	var allData []historyDataPoint

	for {
		if ctx.Err() != nil {
			rs.logger.Info("Data fetch cancelled")
			return
		}

		rs.logger.Debugf("Fetching data page (last: %s)...", filterOpts.Last)

		// Fetch a page of data
		dataResponse, err := dataClient.TabularDataByFilter(ctx, filterOpts)
		if err != nil {
			rs.logger.Errorf("Failed to fetch tabular data: %v", err)
			return
		}

		// Process the data points
		if dataResponse != nil && dataResponse.TabularData != nil {
			for _, dp := range dataResponse.TabularData {
				if dp.Data != nil {
					allData = append(allData, historyDataPoint{
						timestamp: dp.TimeReceived,
						readings:  dp.Data,
					})
				}
			}
		}

		rs.logger.Debugf("Fetched %d data points in this page", len(dataResponse.TabularData))

		// Check if we've fetched all data or hit cache limit
		if dataResponse.Last == "" || len(dataResponse.TabularData) == 0 || len(allData) >= rs.cfg.CacheSize {
			break
		}

		// Update the last token for pagination
		filterOpts.Last = dataResponse.Last
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

	rs.historyData = allData
	rs.currentIndex = 0
	rs.historyStartTime = time.Now()
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

// Readings returns sensor data based on the history timeline
func (rs *historySensor) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	if !rs.isReady || len(rs.historyData) == 0 {
		return map[string]interface{}{
			"status": "loading",
			"ready":  rs.isReady,
		}, nil
	}

	// Calculate elapsed time with speed multiplier
	elapsed := time.Since(rs.historyStartTime)
	if rs.cfg.SpeedMultiplier != 1.0 {
		elapsed = time.Duration(float64(elapsed) * rs.cfg.SpeedMultiplier)
	}

	// Handle looping
	if rs.cfg.Loop && len(rs.historyData) > 1 {
		firstTimestamp := rs.historyData[0].timestamp
		lastTimestamp := rs.historyData[len(rs.historyData)-1].timestamp
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
				rs.logger.Infof("Completed loop %d, restarting history", loops)
				rs.mu.Unlock()
				rs.mu.RLock()
			}
		}
	}

	// Calculate the current position in historical time
	currentHistoricalTime := rs.historyData[0].timestamp.Add(elapsed)

	// Find the appropriate reading for the current time
	var foundReading map[string]interface{}
	foundIndex := rs.currentIndex

	for i := rs.currentIndex; i < len(rs.historyData); i++ {
		dp := rs.historyData[i]
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
			"next_data_in": rs.historyData[0].timestamp.Sub(currentHistoricalTime).Seconds(),
		}, nil
	}

	// Check if the data has "readings" wrapper and unwrap it
	result := make(map[string]interface{})

	if readings, ok := foundReading["readings"].(map[string]interface{}); ok {
		for k, v := range readings {
			result[k] = v
		}
	} else {
		for k, v := range foundReading {
			result[k] = v
		}
	}

	// Add history metadata if configured or requested via extra
	includeMetadata := rs.cfg.IncludeMetadata || (extra != nil && extra["include_metadata"] == true)
	if includeMetadata {
		result["_history_metadata"] = map[string]interface{}{
			"historical_timestamp": rs.historyData[foundIndex].timestamp.Format(time.RFC3339),
			"history_position":     fmt.Sprintf("%d/%d", foundIndex+1, len(rs.historyData)),
			"loop_count":           rs.loopCount,
			"speed_multiplier":     rs.cfg.SpeedMultiplier,
		}
	}

	return result, nil
}

// DoCommand implements custom commands for the sensor
func (rs *historySensor) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	command, ok := cmd["command"].(string)
	if !ok {
		return nil, errors.New("command field is required")
	}

	switch command {
	case "get_stats":
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

	case "reset":
		rs.mu.Lock()
		defer rs.mu.Unlock()

		rs.currentIndex = 0
		rs.loopCount = 0
		rs.historyStartTime = time.Now()
		rs.logger.Info("History reset to beginning")

		return map[string]interface{}{"status": "reset"}, nil

	case "jump_to_percent":
		percent, ok := cmd["percent"].(float64)
		if !ok {
			if percentInt, ok := cmd["percent"].(int); ok {
				percent = float64(percentInt)
			} else {
				return nil, errors.New("percent parameter is required and must be a number between 0 and 100")
			}
		}

		if percent < 0 || percent > 100 {
			return nil, errors.New("percent must be between 0 and 100")
		}

		rs.mu.Lock()
		defer rs.mu.Unlock()

		if len(rs.historyData) > 0 {
			targetIndex := int(float64(len(rs.historyData)-1) * (percent / 100.0))
			rs.currentIndex = targetIndex

			// Adjust history start time to match the jump
			firstTime := rs.historyData[0].timestamp
			targetTime := rs.historyData[targetIndex].timestamp
			timeOffset := targetTime.Sub(firstTime)
			rs.historyStartTime = time.Now().Add(-timeOffset)

			rs.logger.Infof("Jumped to %d%% (index %d)", int(percent), targetIndex)

			return map[string]interface{}{
				"status":    "jumped",
				"index":     targetIndex,
				"timestamp": targetTime.Format(time.RFC3339),
			}, nil
		}

		return map[string]interface{}{"status": "no_data"}, nil

	default:
		return nil, errors.Errorf("unknown command: %s", command)
	}
}

// Close gracefully shuts down the sensor
func (rs *historySensor) Close(ctx context.Context) error {
	rs.logger.Info("Closing history sensor")

	rs.mu.Lock()
	defer rs.mu.Unlock()

	if rs.cancelFunc != nil {
		rs.cancelFunc()
		rs.cancelFunc = nil
	}

	// Log final statistics
	if rs.stats.totalDataPoints > 0 {
		rs.logger.Infof("History sensor closed. Processed %d data points, completed %d loops",
			rs.stats.totalDataPoints, rs.stats.loopsCompleted)
	}

	return nil
}
