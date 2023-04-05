package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"

	// "math"
	"math/rand"
	"os"

	// "strings"
	"sync"
	"sync/atomic"

	"time"

	"github.com/montanaflynn/stats"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

var wsErors int64 = 0	   // Суммарное количетво ошибок вебсокета
var sampleRate = 48000
var rescore = false
var duration int           // Продолжительность работы теста в минутах
var pauseMin = 0		   // Нижняя граница для рандома паузы между запросами
var pauseMax = 0           // Верхняя граница для рандома паузы между запросами

// Вывод сообщения воркера
func workerPrint(msg string, workerNum int) {
	fmt.Printf("Worker: %d, Msg: %s\n", workerNum, msg)
}

func timer(sendChan chan int, recvChan chan int, timeChan chan time.Duration) {
	for i := 1; i <= 4; i++ {
		time.Sleep(time.Duration(250) * time.Millisecond)
		sendChan <- i // Отпрвка сигнала для поссылки чанка
	}
	close(sendChan)

	start := time.Now()

	<- recvChan // Получение сигнала о завершении

	timeChan <- time.Since(start) // Отправка итогового времени
	close(timeChan)
}

// Отправка pcm аудиофайла на сервер
func sendPcm(audio []byte, host string, workerNum int) (string, time.Duration, error) {
	ctx := context.Background()

	recvChan := make(chan int, 4) // канал для получения сигналов на поссылку чанка
	sendChan := make(chan int) // канал для отправки сигнала о завершении распознования
	timeChan := make(chan time.Duration)
	
	go timer(recvChan, sendChan, timeChan)

	conn, _, err := websocket.Dial(ctx, host, nil)
	if err != nil {
		workerPrint(err.Error(), workerNum)
		return "", time.Duration(0), err
	}
	defer conn.Close(websocket.StatusInternalError, "")
	type Conf struct {
		SampleRate int  `json:"sample_rate"`
		Rescore    bool `json:"use_rescoring"`
	}
	var req = struct {
		Config Conf `json:"config"`
	}{Config: Conf{SampleRate: sampleRate, Rescore: rescore}}
	if err := wsjson.Write(ctx, conn, req); err != nil {
		workerPrint(err.Error(), workerNum)
		return "", time.Duration(0), err
	}

	reader := bytes.NewReader(audio)

	buf := make([]byte, int(float64(sampleRate * 2) * 0.25)) // буфер равный 250 мс
	var respJson map[string]interface{}

	for {
		n, err := reader.Read(buf)
		if err != nil {
			close(sendChan)
			return "", time.Duration(0), err
		}

		i := <- recvChan

		err = conn.Write(ctx, websocket.MessageBinary, buf[:n])
		if err != nil {
			workerPrint(err.Error(), workerNum)
			close(sendChan)
			return "", time.Duration(0), err
		}

		err = wsjson.Read(ctx, conn, &respJson)
		if err != nil {
			workerPrint(err.Error(), workerNum)
			close(sendChan)
			return "", time.Duration(0), err
		}

		workerPrint(fmt.Sprintf("%d/4: %#v", i, respJson), workerNum)

		if i == 4 {
			if respJson["status"] == "result" {
				sendChan <- 0
				close(sendChan)
				break
			}

			err = conn.Write(ctx, websocket.MessageBinary, buf[:n])
			if err != nil {
				workerPrint(err.Error(), workerNum)
				close(sendChan)
				return "", time.Duration(0), err
			}

			err = wsjson.Read(ctx, conn, &respJson)
			if err != nil {
				workerPrint(err.Error(), workerNum)
				close(sendChan)
				return "", time.Duration(0), err
			}

			sendChan <- 0
			close(sendChan)
			break
		}
	}
	conn.Close(websocket.StatusNormalClosure, "")

	elapsedTime := <- timeChan

	return fmt.Sprintf("%#v", respJson), elapsedTime, nil
}

// Запуск воркера, итерантивно отправляющего аудио на распознование в течении duration минут.
// Каждый последующий запрос отправляется с задержкой 0-50ms
// func workerProc(audio []byte, host string, workerNum int, wg *sync.WaitGroup, maxChan chan int64, minChan chan int64, timeChan chan []int64) {
func workerProc(audio []byte, host string, workerNum int, wg *sync.WaitGroup, timeChan chan []int64) {
	defer wg.Done()

	timesList := make([]int64, 0, duration * 60)

	end := time.Now().Add(time.Duration(duration) * time.Minute)
	runLoop := true
	for runLoop {
		for runLoop {	
			if time.Now().After(end) {
				runLoop = false
			}

			time.Sleep(time.Duration(rand.Intn(pauseMax) + pauseMin) * time.Millisecond)
			if respJson, elapsedTime, err := sendPcm(audio, host, workerNum); err == nil {
				respText := fmt.Sprintf("%s, %dms", respJson, elapsedTime.Milliseconds())
				workerPrint(respText, workerNum)

				timesList = append(timesList, elapsedTime.Milliseconds())
				break
			} else {
				workerPrint(err.Error(), workerNum)
			}
			atomic.AddInt64(&wsErors, 1)
		}
	}
	timeChan <- timesList
}

func main() {
	rand.Seed(time.Now().UnixNano())	
	var wg sync.WaitGroup
	var filename, host string
	var numWorkers int
	var csvFilename string
	var avgWsErrors int64

	flag.StringVar(&filename, "filename", "", "Path to pcm file")
	flag.StringVar(&host, "host", "", "Host adreess with port (e.g. localhost:2700)")
	flag.IntVar(&numWorkers, "worker", 1, "Workers count")
	flag.IntVar(&duration, "duration", 30, "Test duration in mins")
	flag.IntVar(&sampleRate, "sr", 48000, "Samplerate")
	flag.BoolVar(&rescore, "rescore", false, "Use rescore")
	flag.IntVar(&pauseMin, "pause_min", 1, "Low border of random for pause duration in ms")
	flag.IntVar(&pauseMax, "pause_max", 50, "High border of random for pause duration in ms")
	flag.StringVar(&csvFilename, "csv", "", "Path to output csv file (creates new or append string to existing one)")
	flag.Parse()

	timeChan := make(chan []int64, numWorkers)
	
	fmt.Println(filename, host, numWorkers, sampleRate, rescore)
	wsAsrHost := fmt.Sprintf("ws://%s", host)
	f, err := os.Open(filename)
	if err != nil {
		log.Fatal(err)
	}
	audio := make([]byte, 1024)
	buf := make([]byte, 1024)
	for {
		_, err := f.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatal(err)
		}
		audio = append(audio, buf...)
	}
	log.Println("Start testing")
	for wN := 1; wN <= numWorkers; wN++ {
		log.Printf("Start worker: %d\n", wN)
		wg.Add(1)
		go workerProc(audio, wsAsrHost, wN, &wg, timeChan)
	}
	wg.Wait()
	close(timeChan)

	fullTimesList := make([]int64, 0, duration * numWorkers * 60)
	for timesList := range timeChan {
		fullTimesList = append(fullTimesList, timesList...)
	}
	
	reqCount := int64(len(fullTimesList))

	data := stats.LoadRawData(fullTimesList)
	
	maxTime, _ := data.Max()
	minTime, _ := data.Min()
	avgTime, _ := data.Mean()
	medTime, _ := data.Median()
	totalTime, _ := data.Sum()

	log.Printf("Completed: %d\n", reqCount)
	log.Printf("All requests time: %.0fms, Average request time: %.0fms, Median request time %.0fms, Max request time: %.0fms, min request time: %.0fms\n", totalTime, avgTime, medTime, maxTime, minTime)
	
	if reqCount > 0 {
		avgWsErrors = wsErors / reqCount
		log.Printf("Errors count: %d, Average errors per request: %d\n", wsErors, avgWsErrors)
	}

	if csvFilename != "" {
		writeCsvString := fmt.Sprintf("%s;%d;%.0fms;%.0fms;%.0fms;%.0fms;%d;%dmin;%d\n", host, numWorkers, avgTime, medTime, maxTime, minTime, reqCount, duration, avgWsErrors)
		
		if _, err := os.Stat(csvFilename); err != nil {
			if os.IsNotExist(err) {
				log.Print("Create new file")
				writeCsvString = fmt.Sprint("host;workers;avg;median;max;min;reqs;duration;avgwserrors\n", writeCsvString)
			}else {
				log.Fatal(err)
			}
		}

		log.Printf("Write res to %s\n", csvFilename)
		f, err := os.OpenFile(csvFilename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Fatal(err)
		}
		defer func() {
			if err := f.Close(); err != nil {
				log.Fatal(err)
			}
		}()
		_, err = f.WriteString(writeCsvString)
		if err != nil {
			log.Fatal(err)
		}
	}
}