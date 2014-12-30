// Copyright (C) 2014 Ian Bishop
//
// This program is free software; you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation; either version 2 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License along
// with this program; if not, write to the Free Software Foundation, Inc.,
// 51 Franklin Street, Fifth Floor, Boston, MA 02110-1301 USA.

// Dockerinflux populates InfluxDB backend with metrics (CPU, memory) from Docker
// containers running on localhost. These can then be graphed using a frontend
// like Grafana.
//
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	influxdb "github.com/influxdb/influxdb/client"
	cstat "github.com/porjo/gocstat"
	"github.com/samalba/dockerclient"
)

const appName = "dockerinflux"
const influxWriteInterval = 30 * time.Second
const influxWriteLimit = 50
const packetChannelSize = 100

var (
	logPath *string
	verbose *bool

	cgroupsPath *string

	// influxdb options
	host      *string
	username  *string
	password  *string
	database  *string
	normalize *bool

	docker *dockerT

	client      *influxdb.Client
	beforeCache map[string]CacheEntry
)

type dockerT struct {
	sync.Mutex
	client *dockerclient.DockerClient
	names  map[string]string
}

// point cache to perform data normalization for COUNTER and DERIVE types
type CacheEntry struct {
	Timestamp time.Time
	Value     uint64
}

// signal handler
func handleSignals(c chan os.Signal) {
	// block until a signal is received
	sig := <-c

	log.Printf("exit with a signal: %v\n", sig)
	os.Exit(1)
}

func init() {
	log.SetPrefix("[" + appName + "] ")

	logPath = flag.String("logfile", "", "path to log file")
	verbose = flag.Bool("verbose", false, "turn on debug output")

	cgroupsPath = flag.String("cgroups", "/sys/fs/cgroup", "location of cgroups directory")

	// influxdb options
	host = flag.String("influxdb", "localhost:8086", "host:port for InfluxDB")
	username = flag.String("username", "root", "username for InfluxDB")
	password = flag.String("password", "root", "password for InfluxDB")
	database = flag.String("database", "dockerinflux", "database for InfluxDB")

	// docker options
	dockerSock := flag.String("docker", "", "Docker socket used to resolve container IDs to friendly names e.g. unix:///var/run/docker.sock (optional)")

	flag.Parse()

	if *dockerSock != "" {
		docker = &dockerT{}
		// Init the client
		dc, err := dockerclient.NewDockerClient(*dockerSock, nil)
		if err != nil {
			log.Fatal(err)
		}
		docker.client = dc
		go func() {
			for {
				if err := docker.updateNames(); err != nil {
					log.Printf("Error updating Docker container names, err '%s'\n", err)
				}
				time.Sleep(1 * time.Minute)
				if *verbose {
					log.Printf("[TRACE] updating docker container list\n")
				}
			}
		}()
	}

	beforeCache = make(map[string]CacheEntry)
}

func main() {
	var err error

	if *logPath != "" {
		var logFile *os.File
		logFile, err = os.OpenFile(*logPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			log.Fatalf("failed to open file: %v\n", err)
		}
		log.SetOutput(logFile)
		defer logFile.Close()
	}

	log.Printf("Starting\n")

	// make influxdb client
	client, err = influxdb.NewClient(&influxdb.ClientConfig{
		Host:     *host,
		Username: *username,
		Password: *password,
		Database: *database,
	})
	if err != nil {
		log.Fatalf("failed to make a influxdb client: %v\n", err)
	}

	err = client.Ping()
	if err != nil {
		time.Sleep(5 * time.Second)
		err = client.Ping()
		if err != nil {
			log.Fatalf("failed to reach influxdb backend: %v\n", err)
		}
	}

	// Create DB if not exists
	createDb := true
	dbs, _ := client.GetDatabaseList()
	if _, ok := dbs[*database]; ok {
		createDb = false
	}

	if createDb {
		log.Printf("database '%s' not found, creating...\n", *database)
		if err = client.CreateDatabase(*database); err != nil {
			log.Fatalf("error creating database '%s', %s\n", *database, err)
		}
	}

	// register a signal handler
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, os.Interrupt, os.Kill)
	go handleSignals(sc)

	errChan := make(chan error)
	cstat.BasePath = *cgroupsPath
	err = cstat.Init(errChan)
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		err := readStats()
		if err != nil {
			errChan <- err
		}
	}()

	// block waiting for channel
	err = <-errChan
	if err != nil {
		log.Fatalf("errChan %s\n", err)
	}
}

func readStats() error {
	seriesGroup := make([]*influxdb.Series, 0)
	timer := time.Now()
	for {
		if *verbose {
			log.Printf("[TRACE] reading stats\n")
		}
		stats, err := cstat.ReadStats()
		if err != nil {
			return err
		}
		for id, stat := range stats {
			hostname := id
			// Trim container id
			if len(id) >= 12 {
				hostname = id[0:12]
			}
			// Try and resolve Docker container ID to real name
			if docker != nil {
				docker.Lock()
				if realName, ok := docker.names[id]; ok {
					hostname = realName
				}
				docker.Unlock()
			}
			seriesGroup = append(seriesGroup, processStat(hostname, stat.Memory)...)
			seriesGroup = append(seriesGroup, processStat(hostname, stat.CPU)...)
			seriesGroup = append(seriesGroup, processStat(hostname, stat.BlkIO)...)
			if *verbose {
				log.Printf("[TRACE] got seriesGroup %v\n", seriesGroup)
			}
			if len(seriesGroup) >= influxWriteLimit || time.Since(timer) >= influxWriteInterval {
				go backendWriter(seriesGroup)
				seriesGroup = make([]*influxdb.Series, 0)
				timer = time.Now()
			}
		}
		time.Sleep(influxWriteInterval)
	}
}

func backendWriter(seriesGroup []*influxdb.Series) {
	if err := client.WriteSeries(seriesGroup); err != nil {
		log.Printf("failed to write series group to influxdb: %s\n", err)
	}
	if *verbose {
		log.Printf("[TRACE] wrote %d series\n", len(seriesGroup))
	}
}

func processStat(hostname string, stat interface{}) []*influxdb.Series {
	if *verbose {
		log.Printf("[TRACE] got stat: %v\n", stat)
	}

	var seriesGroup []*influxdb.Series

	var name, cacheKey string
	switch t := stat.(type) {
	case cstat.MemStat:
		name = "Memory.RSS"
		cacheKey = hostname + "." + name
		seriesGroup = append(seriesGroup, genSeries(name, hostname, cacheKey, t.RSS, t.Timestamp, false))
		name = "Memory.Cache"
		cacheKey = hostname + "." + name
		seriesGroup = append(seriesGroup, genSeries(name, hostname, cacheKey, t.Cache, t.Timestamp, false))
	case cstat.CPUStat:
		name = "CPU.User"
		cacheKey = hostname + "." + name
		seriesGroup = append(seriesGroup, genSeries(name, hostname, cacheKey, t.User, t.Timestamp, true))
		name = "CPU.System"
		cacheKey = hostname + "." + name
		seriesGroup = append(seriesGroup, genSeries(name, hostname, cacheKey, t.System, t.Timestamp, true))
	case cstat.BlkIOStat:
		for _, dev := range t.Bytes.Devices {
			name = fmt.Sprintf("BlkIO.Bytes.%d.%d.Read", dev.Major, dev.Minor)
			cacheKey = hostname + "." + name
			seriesGroup = append(seriesGroup, genSeries(name, hostname, cacheKey, dev.Read, t.Bytes.Timestamp, true))
			name = fmt.Sprintf("BlkIO.Bytes.%d.%d.Write", dev.Major, dev.Minor)
			cacheKey = hostname + "." + name
			seriesGroup = append(seriesGroup, genSeries(name, hostname, cacheKey, dev.Write, t.Bytes.Timestamp, true))
		}
		for _, dev := range t.IOPS.Devices {
			name = fmt.Sprintf("BlkIO.IOPS.%d.%d.Read", dev.Major, dev.Minor)
			cacheKey = hostname + "." + name
			seriesGroup = append(seriesGroup, genSeries(name, hostname, cacheKey, dev.Read, t.Bytes.Timestamp, true))
			name = fmt.Sprintf("BlkIO.IOPS.%d.%d.Write", dev.Major, dev.Minor)
			cacheKey = hostname + "." + name
			seriesGroup = append(seriesGroup, genSeries(name, hostname, cacheKey, dev.Write, t.Bytes.Timestamp, true))
		}
	}

	return seriesGroup
}

func genSeries(name, hostName, cacheKey string, value uint64, timestamp time.Time, delta bool) *influxdb.Series {
	deltaValue := value
	if delta {
		if before, ok := beforeCache[cacheKey]; ok {
			deltaValue = value - before.Value
		}
		entry := CacheEntry{
			Timestamp: timestamp,
			Value:     value,
		}
		beforeCache[cacheKey] = entry
	}

	// InfluxDB takes timestamps as milliseconds by default
	timestampMs := timestamp.UnixNano() / 1000000

	series := &influxdb.Series{
		Name:    name,
		Columns: []string{"time", "value", "host"},
		Points: [][]interface{}{
			[]interface{}{timestampMs, deltaValue, hostName},
		},
	}
	if *verbose {
		log.Printf("[TRACE] ready to send series: %v\n", series)
	}
	return series
}

func (d *dockerT) updateNames() error {
	containers, err := d.client.ListContainers(true, true, "")
	if err != nil {
		return err
	}
	d.Lock()
	defer d.Unlock()
	d.names = make(map[string]string)
	for _, c := range containers {
		info, err := d.client.InspectContainer(c.Id)
		if err != nil {
			return err
		}

		if info.State.Running {
			d.names[c.Id] = strings.TrimPrefix(c.Names[0], "/")
		}
	}
	if *verbose {
		log.Printf("[TRACE] docker names updated: %v\n", d.names)
	}

	return nil
}
