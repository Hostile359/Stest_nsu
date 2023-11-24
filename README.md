# Stest nsu

Program for stress test online ASR websocket service.
All test audio files should be converted to pcm before tests and their length should be multiple by 250ms.

## Requirements
  go 1.18+

## Compile

```bash
go build -o ./bin/prog ./cmd/main.go
```

## Run with help message
```bash
./bin/prog -help
```

## Run example
```bash
./bin/prog -pcmpath pcm_dir/ -host localhost:2700 -sr 16000 -duration 5 -worker 500 -csv out.csv -res_file out.json
```

## Run without compile, using go
```bash
go run ./cmd/main.go -pcmpath pcm_dir/ -host localhost:2700 -sr 16000 -duration 5 -worker 500 -csv out.csv -res_file out.json
```

## Prety print csv in terminal
```bash
cat <csvfile> | column -t -s ';'
```
## Monitoring with grafana

You can visualize client latency with grafana and prometheus. It has a default dashboard which shows 99, 90 and 50 percentiles for client latency.

Run monitoring services
```bash
docker-compose -f monitoring/docker-compose.yml up -d
```
Down monitoring services
```bash
docker-compose -f monitoring/docker-compose.yml down
```
Default credentials is username: "admin", password: "admin"
