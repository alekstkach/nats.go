// Copyright 2012 Apcera Inc. All rights reserved.

package main

import (
	"flag"
	"strings"

	"github.com/apcera/gnatsd/server"
)

func main() {
	// logging setup
	server.LogSetup()

	// Server Options
	opts := server.Options{}

	var showVersion bool
	var debugAndTrace bool
	var configFile string

	// Parse flags
	flag.IntVar(&opts.Port, "port", server.DEFAULT_PORT, "Port to listen on.")
	flag.IntVar(&opts.Port, "p", server.DEFAULT_PORT, "Port to listen on.")
	flag.StringVar(&opts.Host, "addr", server.DEFAULT_HOST, "Network host to listen on.")
	flag.StringVar(&opts.Host, "a", server.DEFAULT_HOST, "Network host to listen on.")
	flag.StringVar(&opts.Host, "net", server.DEFAULT_HOST, "Network host to listen on.")
	flag.BoolVar(&opts.Debug, "D", false, "Enable Debug logging.")
	flag.BoolVar(&opts.Debug, "debug", false, "Enable Debug logging.")
	flag.BoolVar(&opts.Trace, "V", false, "Enable Trace logging.")
	flag.BoolVar(&opts.Trace, "trace", false, "Enable Trace logging.")
	flag.BoolVar(&debugAndTrace, "DV", false, "Enable Debug and Trace logging.")
	flag.BoolVar(&opts.Logtime, "T", true, "Timestamp log entries.")
	flag.BoolVar(&opts.Logtime, "logtime", true, "Timestamp log entries.")
	flag.StringVar(&opts.Username, "user", "", "Username required for connection.")
	flag.StringVar(&opts.Password, "pass", "", "Password required for connection.")
	flag.StringVar(&opts.Authorization, "auth", "", "Authorization token required for connection.")
	flag.IntVar(&opts.HttpPort, "m", 0, "HTTP Port for /varz, /connz endpoints.")
	flag.IntVar(&opts.HttpPort, "http_port", 0, "HTTP Port for /varz, /connz endpoints.")
	flag.StringVar(&configFile, "c", "", "Configuration file.")
	flag.StringVar(&configFile, "config", "", "Configuration file.")
	flag.StringVar(&opts.PidFile, "P", "", "File to store process pid.")
	flag.StringVar(&opts.PidFile, "pid", "", "File to store process pid.")
	flag.StringVar(&opts.LogFile, "l", "", "File to store logging output.")
	flag.StringVar(&opts.LogFile, "log", "", "File to store logging output.")
	flag.BoolVar(&showVersion, "version", false, "Print version information.")
	flag.BoolVar(&showVersion, "v", false, "Print version information.")

	flag.Usage = server.Usage

	flag.Parse()

	// Show version and exit
	if showVersion {
		server.PrintServerAndExit()
	}

	// One flag can set multiple options.
	if debugAndTrace {
		opts.Trace, opts.Debug = true, true
	}

	// Process args looking for non-flaf options,
	// 'version' and 'help' only for now
	for _, arg := range flag.Args() {
		switch strings.ToLower(arg) {
		case "version":
			server.PrintServerAndExit()
		case "help":
			server.Usage()
		}
	}

	var fileOpts *server.Options
	var err error

	// Parse config if given
	if configFile != "" {
		fileOpts, err = server.ProcessConfigFile(configFile)
		if err != nil {
			server.PrintAndDie(err.Error())
		}
	}

	// Create the server with appropriate options.
	s := server.New(server.MergeOptions(fileOpts, &opts))

	// Start things up. Block here til done.
	s.Start()
}