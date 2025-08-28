package main

import (
	sensorplayback "github.com/hunter-volkman/sensor-playback"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"
)

func main() {
	// Create an APIModel that combines the API and Model
	apiModel := resource.APIModel{
		API:   sensor.API,
		Model: sensorplayback.Model,
	}

	// Start the module with our APIModel
	module.ModularMain(apiModel)
}
