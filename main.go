package main

import (
	"github.com/docker/machine/libmachine/drivers/plugin"
	"github.com/edorid/docker-machine-driver-incus/pkg/drivers/incus"
)

func main() {
	plugin.RegisterDriver(incus.NewDriver("", ""))
}
