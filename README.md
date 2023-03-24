# Stest nsu

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
./bin/prog -filename auido.pcm -host localhost:2700 -sr 16000 -duration 5 -worker 500 -csv out.csv
```

## Run without compile, using go
```bash
go run ./cmd/main.go -filename auido.pcm -host localhost:2700 -sr 16000 -duration 5 -worker 500 -csv out.csv
```

## Prety print csv in terminal
```bash
cat <csvfile> | column -t -s ';'
```
