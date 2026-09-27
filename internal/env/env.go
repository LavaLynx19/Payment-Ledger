// Package env reads process configuration. Values are referenced by name only.
package env

import (
	"log"
	"os"
	"strconv"
	"time"
)

// Must returns the variable or exits naming the missing variable.
func Must(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("%s is not set", name)
	}
	return v
}

func Or(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func Int(name string, fallback int) int {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("%s must be an integer: %v", name, err)
	}
	return n
}

func Duration(name string, fallback time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Fatalf("%s must be a duration like 30s: %v", name, err)
	}
	return d
}
