package sensorreplay

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/pkg/errors"
	"go.viam.com/rdk/app"
	"go.viam.com/rdk/app/data"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/utils/rpc"
)

// Model is the model of the sensor-replay component.
var Model = resource.NewModel("hunter", "sensor-replay", "sensor")

// replayDataPoint stores a single historical reading with its timestamp.
type replayDataPoint struct {
	timestamp time.Time
	readings  map[string]interface{}
}

// Config holds the configuration attributes for the replay sensor.
type Config struct {
	SourceComponentName string `json:"source_component_name"`
	SourceComponentType string `json:"source_component_type"`
	OrganizationID      string `json:"organization_id"`
	StartTimeUTC        string `json:"start_time_utc"`
	EndTimeUTC          string `json:"end_time_utc"`
	APIKeyID            string `json:"api_key_id"`
	APIKey              string `json:"api_key"`
	Loop                bool   `json:"loop"`
}

// Validate ensures all required configuration fields are present.
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
	return nil, nil // No dependencies on other components.
}

// replaySensor is the main struct for our component.
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
	cancelCtx       context.Context
	cancelFunc      func()
}

func init() {
	registration := resource.Registration[sensor.Sensor, *Config]{
		Constructor: newReplaySensor,
	}
	resource.RegisterComponent(sensor.API, Model, registration)
}

// newReplaySensor is the constructor for the replay sensor.
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

// Reconfigure handles the initial setup and subsequent configuration updates.
func (rs *replaySensor) Reconfigure(ctx context.Context, deps resource.Dependencies, conf resource.Config) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	// Cancel any previous background tasks.
	if rs.cancelFunc != nil {
		rs.cancelFunc()
	}
	rs.cancelCtx, rs.cancelFunc = context.WithCancel(context.Background())
	rs.isReady = false

	newConfig, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return err
	}
	rs.cfg = newConfig

	// Parse timestamps from config.
	rs.startTime, err = time.Parse(time.RFC3339, newConfig.StartTimeUTC)
	if err != nil {
		return errors.Wrap(err, "failed to parse start_time_utc")
	}
	rs.endTime, err = time.Parse(time.RFC3339, newConfig.EndTimeUTC)
	if err != nil {
		return errors.Wrap(err, "failed to parse end_time_utc")
	}

	// Fetch data in the background to not block startup.
	go rs.fetchAndPrepareData(rs.cancelCtx)

	return nil
}

// fetchAndPrepareData connects to the Viam app, downloads, and prepares the data for replay.
func (rs *replaySensor) fetchAndPrepareData(ctx context.Context) {
	rs.logger.Info("Starting data fetch for replay sensor...")

	// Establish connection to Viam App.
	creds := rpc.Credentials{Type: rpc.CredentialsTypeAPIKey, Payload: rs.cfg.APIKey}
	dialOpts := []rpc.DialOption{rpc.WithEntity(rs.cfg.APIKeyID), rpc.WithCredentials(creds)}
	client, err := app.NewClient(ctx, "app.viam.com:443", dialOpts...)
	if err != nil {
		rs.logger.Errorf("Failed to connect to Viam app: %v", err)
		return
	}
	defer client.Close()

	dataClient := client.DataClient()
	filter := &data.Filter{
		ComponentName: rs.cfg.SourceComponentName,
		ComponentType: rs.cfg.SourceComponentType,
		StartTime:     &rs.startTime,
		EndTime:       &rs.endTime,
	}

	var allData []replayDataPoint
	var last string
	for {
		if ctx.Err() != nil {
			rs.logger.Info("Data fetch cancelled.")
			return
		}
		rs.logger.Infof("Fetching data page... last ID: %s", last)
		resp, err := dataClient.TabularDataByFilter(ctx, filter, data.WithLast(last))
		if err != nil {
			rs.logger.Errorf("Failed to fetch tabular data page: %v", err)
			return
		}

		for _, item := range resp.Data {
			readings, ok := item.Data["readings"].(map[string]interface{})
			if ok {
				allData = append(allData, replayDataPoint{
					timestamp: *item.TimeReceived,
					readings:  readings,
				})
			}
		}

		if resp.Last == "" || len(resp.Data) == 0 {
			break
		}
		last = resp.Last
	}

	if len(allData) == 0 {
		rs.logger.Warn("No data found for the specified time range. Readings will be empty.")
		return
	}

	// Sort data chronologically.
	sort.Slice(allData, func(i, j int) bool {
		return allData[i].timestamp.Before(allData[j].timestamp)
	})

	rs.logger.Infof("Successfully fetched and sorted %d data points.", len(allData))

	// Lock to safely update the shared state.
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.replayData = allData
	rs.currentIndex = 0
	rs.replayStartTime = time.Now()
	rs.isReady = true
}

// Readings is the main method that returns sensor data based on the replay timeline.
func (rs *replaySensor) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	if !rs.isReady || len(rs.replayData) == 0 {
		return make(map[string]interface{}), nil
	}

	elapsed := time.Since(rs.replayStartTime)

	// Handle looping
	if rs.cfg.Loop {
		firstTimestamp := rs.replayData[0].timestamp
		lastTimestamp := rs.replayData[len(rs.replayData)-1].timestamp
		totalDuration := lastTimestamp.Sub(firstTimestamp)
		if elapsed > totalDuration {
			rs.logger.Info("Looping replay data.")
			// To be perfectly accurate, we need a write lock to reset state.
			// This is a quick upgrade from read lock to write lock.
			rs.mu.RUnlock()
			rs.mu.Lock()
			rs.replayStartTime = time.Now()
			rs.currentIndex = 0
			elapsed = 0
			rs.mu.Unlock()
			rs.mu.RLock() // Re-acquire read lock
		}
	}

	currentHistoricalTime := rs.replayData[0].timestamp.Add(elapsed)

	var foundReading map[string]interface{}
	for i := rs.currentIndex; i < len(rs.replayData); i++ {
		dp := rs.replayData[i]
		if dp.timestamp.Before(currentHistoricalTime) || dp.timestamp.Equal(currentHistoricalTime) {
			foundReading = dp.readings
			// This is a small state change, but to be thread-safe, we need a write lock.
			// This pattern is okay for infrequent updates.
			if rs.currentIndex != i {
				rs.mu.RUnlock()
				rs.mu.Lock()
				rs.currentIndex = i
				rs.mu.Unlock()
				rs.mu.RLock()
			}
		} else {
			break
		}
	}

	if foundReading == nil {
		// If we haven't found a reading yet, it means we're before the first data point.
		// Return an empty map.
		return make(map[string]interface{}), nil
	}

	return foundReading, nil
}

// DoCommand and Close are required by the Sensor interface.
func (rs *replaySensor) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	return nil, errors.New("DoCommand not implemented")
}

func (rs *replaySensor) Close(ctx context.Context) error {
	rs.logger.Info("Closing replay sensor.")
	if rs.cancelFunc != nil {
		rs.cancelFunc()
	}
	return nil
}
