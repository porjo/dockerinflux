## Dockerinflux

Dockerinflux populates [InfluxDB](http://github.com/influxdb/influxdb) backend with metrics (CPU, memory) from Docker containers running on localhost. These can then be graphed using a frontend like [Grafana](http://github.com/grafana/grafana)

### Usage


```
./dockerinflux --help
Usage of ./dockerinflux:
  -cgroups="/sys/fs/cgroup": location of cgroups directory
  -database="": database for InfluxDB
  -docker="": Docker socket used to resolve container IDs to friendly names e.g. unix:///var/run/docker.sock (optional)
  -influxdb="localhost:8086": host:port for InfluxDB
  -logfile="": path to log file
  -password="root": password for InfluxDB
  -username="root": username for InfluxDB
  -verbose=false: true if you need to trace the requests
```
