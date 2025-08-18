package main

import (
	sensorreplay "sensor-replay"

	"github.com/hunter-volkman/sensor-replay"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/module"
)

func main() {
	err := module.AddModularResource(sensor.API, sensorreplay.Model)
	if err != nil {
		panic(err)
	}
	module.ModularMain()
}
