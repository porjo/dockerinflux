package main

import (
	"flag"
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
const influxWriteInterval = time.Second
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

	logPath = flag.String("logfile", appName+".log", "path to log file")
	verbose = flag.Bool("verbose", false, "true if you need to trace the requests")

	cgroupsPath = flag.String("cgroups", "/sys/fs/cgroups", "location of cgroups directory")

	// influxdb options
	host = flag.String("influxdb", "localhost:8086", "host:port for influxdb")
	username = flag.String("username", "root", "username for influxdb")
	password = flag.String("password", "root", "password for influxdb")
	database = flag.String("database", "", "database for influxdb")

	// docker options
	dockerSock := flag.String("docker", "", "Docker socket used to resolve container IDs to friendly names e.g. unix:///var/run/docker.sock")

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
	logFile, err := os.OpenFile(*logPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalf("failed to open file: %v\n", err)
	}
	//log.SetOutput(logFile)
	defer logFile.Close()

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

	/*
		err = client.Ping()
		if err != nil {
			log.Fatalf("failed to reach influxdb backend: %v\n", err)
		}
	*/

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
	for {
		if *verbose {
			log.Printf("[TRACE] reading stats\n")
		}
		time.Sleep(influxWriteInterval)
		stats, err := cstat.ReadStats()
		if err != nil {
			return err
		}
		for id, stat := range stats {
			hostname := id
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
			if *verbose {
				log.Printf("[TRACE] got seriesGroup %v\n", seriesGroup)
			}
		}
		if len(seriesGroup) > 0 {
			go backendWriter(seriesGroup)
			seriesGroup = make([]*influxdb.Series, 0)
		}
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

	switch t := stat.(type) {
	case cstat.MemStat:
		log.Printf("memstat\n")
		mem := stat.(cstat.MemStat)
		name := "Memory.RSS"
		cacheKey := hostname + "." + name
		seriesGroup = append(seriesGroup, genSeries(name, hostname, cacheKey, mem.RSS, mem.Timestamp, false))
		name = "Memory.Cache"
		cacheKey = hostname + "." + name
		seriesGroup = append(seriesGroup, genSeries(name, hostname, cacheKey, mem.Cache, mem.Timestamp, false))
	case cstat.CPUStat:
		log.Printf("cpustat\n")
		cpu := stat.(cstat.CPUStat)
		name := "CPU.User"
		cacheKey := hostname + "." + name
		seriesGroup = append(seriesGroup, genSeries(name, hostname, cacheKey, t.User, cpu.Timestamp, true))
		name = "CPU.System"
		cacheKey = hostname + "." + name
		seriesGroup = append(seriesGroup, genSeries(name, hostname, cacheKey, t.System, cpu.Timestamp, true))
	}

	return seriesGroup
}

func genSeries(name, hostName, cacheKey string, value uint64, timestamp time.Time, normalize bool) *influxdb.Series {
	normalizedValue := value
	if normalize {
		if before, ok := beforeCache[cacheKey]; ok {
			normalizedValue = value - before.Value
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
			[]interface{}{timestampMs, normalizedValue, hostName},
		},
	}
	if *verbose {
		log.Printf("[TRACE] ready to send series: %v\n", series)
	}
	return series
}

func (d *dockerT) updateNames() error {
	containers, err := d.client.ListContainers(true)
	if err != nil {
		return err
	}
	d.Lock()
	defer d.Unlock()
	docker.names = make(map[string]string)
	for _, c := range containers {
		d.names[c.Id] = strings.TrimPrefix(c.Names[0], "/")
	}
	return nil
}
