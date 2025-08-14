package main

import (
	sensorreplay "sensor-replay"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/module"
)

func main() {
	err := module.Main(sensor.API, sensorreplay.Model)
	if err != nil {
		panic(err)
	}
}
