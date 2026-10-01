// Package env reads process configuration. Values are referenced by name only.
package env

import (
	"log"
	"os"
	"strconv"
	"strings"
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

// ShardURLs reads the comma-separated SHARD_URLS (shard 0 first), falling
// back to DATABASE_URL as a single shard. It exits if neither is set.
func ShardURLs() []string {
	var urls []string
	for _, u := range strings.Split(os.Getenv("SHARD_URLS"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			urls = append(urls, u)
		}
	}
	if len(urls) == 0 {
		urls = []string{Must("DATABASE_URL")}
	}
	return urls
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
