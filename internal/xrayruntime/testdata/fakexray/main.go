package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

type testConfig struct {
	FailStart bool `json:"failStart"`
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println("Xray test-runtime")
		return
	}
	if len(os.Args) < 4 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: fake-xray run [-test] -config PATH")
		os.Exit(2)
	}
	testOnly := false
	configPath := ""
	for i := 2; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "-test":
			testOnly = true
		case "-config":
			i++
			if i < len(os.Args) {
				configPath = os.Args[i]
			}
		}
	}
	if configPath == "" {
		fmt.Fprintln(os.Stderr, "missing -config")
		os.Exit(2)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var config testConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if testOnly {
		return
	}
	if config.FailStart {
		fmt.Fprintln(os.Stderr, "synthetic Xray startup failure")
		os.Exit(1)
	}
	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, syscall.SIGTERM, syscall.SIGINT)
	<-stopped
}
