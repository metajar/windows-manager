//go:build !windows

package main

import (
	"errors"
	"log"
)

// The brain/face split is a Windows feature. On other platforms these modes are
// unavailable; the standalone mode (and the dev overlay stub) still work for
// exercising the networking on Linux/macOS.

func runService(cfg config, configPath string) {
	log.Fatal("the brain service (-service) is only supported on Windows")
}

func runFace(configPath string) {
	log.Fatal("the face UI (-face) is only supported on Windows")
}

func installService(cfg config, configPath string) error {
	return errors.New("the brain service is only supported on Windows")
}

func uninstallService(configPath string) error {
	return errors.New("the brain service is only supported on Windows")
}
