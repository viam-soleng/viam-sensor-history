package main

import (
    sensorreplay "github.com/hunter-volkman/sensor-replay"
    "go.viam.com/rdk/components/sensor"
    "go.viam.com/rdk/module"
)

func main() {
    // Use the module.Module interface for v0.88.1
    module.ModularMain(sensorreplay.Model, sensor.API)
}
