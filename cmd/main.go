package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"

	// "strings"
	"sync"
	"sync/atomic"

	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

var succes int64 = 0       // Количество успешных запросов
var reqCount int64 = 0     // Общее количество запросов
var totalReqTime int64 = 0 // Суммарное время успешных запросов
var sampleRate = 48000
var rescore = false
var duration int           // Продолжительность работы теста в минутах
var pauseMin = 0		   // Нижняя граница для рандома паузы между запросами
var pauseMax = 0           // Верхняя граница для рандома паузы между запросами

// Вывод сообщения воркера
func workerPrint(msg string, workerNum int) {
	fmt.Printf("Worker: %d, Msg: %s\n", workerNum, msg)
}

// Отправка pcm аудиофайла на сервер
func sendPcm(audio []byte, host string, workerNum int) {
	ctx := context.Background()

	start := time.Now()
	conn, _, err := websocket.Dial(ctx, host, nil)
	if err != nil {
		workerPrint(err.Error(), workerNum)
		return
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
		return
	}

	var respText string
	reader := bytes.NewReader(audio)

	for {
		buf := make([]byte, sampleRate)
		n, err := reader.Read(buf)
		if err == nil {
			err = conn.Write(ctx, websocket.MessageBinary, buf[:n])
			if err != nil {
				workerPrint(err.Error(), workerNum)
				return
			}
		} else if err == io.EOF {
			err = conn.Write(ctx, websocket.MessageText, []byte("eof"))
			if err != nil {
				workerPrint(err.Error(), workerNum)
				return
			}
			break
		} else {
			workerPrint(err.Error(), workerNum)
			return
		}

		_, _, err = conn.Read(ctx)
		if err != nil {
			workerPrint(err.Error(), workerNum)
			return
		}
	}

	type Resp struct {
		AsrResult     string  `json:"asr_result"`
		RescResult    string  `json:"rescoring_result"`
		CommandResult string  `json:"command"`
		Time          float64 `json:"time"`
	}
	var respJson Resp
	err = wsjson.Read(ctx, conn, &respJson)
	if err != nil {
		workerPrint(err.Error(), workerNum)
		return
	}
	conn.Close(websocket.StatusNormalClosure, "")
	elapsedTime := time.Since(start).Milliseconds()

	respText = fmt.Sprintf("%#v, %dms", respJson, elapsedTime)
	workerPrint(respText, workerNum)

	atomic.AddInt64(&succes, 1)
	atomic.AddInt64(&totalReqTime, elapsedTime)

}

// Запуск воркера, итерантивно отправляющего аудио на распознование в течении duration минут.
// Каждый последующий запрос отправляется с задержкой 0-50ms
func workerProc(audio []byte, host string, workerNum int, wg *sync.WaitGroup) {
	defer wg.Done()

	end := time.Now().Add(time.Duration(duration) * time.Minute)
	for {
		if time.Now().After(end) {
			break
		}
		
		time.Sleep(time.Duration(rand.Intn(pauseMax) + pauseMin) * time.Millisecond)
		atomic.AddInt64(&reqCount, 1)
		sendPcm(audio, host, workerNum)
	}
}

func main() {
	rand.Seed(time.Now().UnixNano())	
	var wg sync.WaitGroup
	var filename, host string
	var numWorkers int

	flag.StringVar(&filename, "filename", "", "Path to pcm file")
	flag.StringVar(&host, "host", "", "Host adreess with port (e.g. localhost:2700)")
	flag.IntVar(&numWorkers, "worker", 1, "Workers count")
	flag.IntVar(&duration, "duration", 30, "Test duration in mins")
	flag.IntVar(&sampleRate, "sr", 48000, "Samplerate")
	flag.BoolVar(&rescore, "rescore", false, "Use rescore")
	flag.IntVar(&pauseMin, "pause_min", 1, "Low border of random for pause duration in ms")
	flag.IntVar(&pauseMax, "pause_max", 50, "High border of random for pause duration in ms")
	flag.Parse()

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
		go workerProc(audio, wsAsrHost, wN, &wg)
	}
	wg.Wait()

	log.Printf("Completed: %d/%d\n", succes, reqCount)
	if succes > 0 {
		log.Printf("All SUCCESED requests time: %dms, Average request time: %dms\n", totalReqTime, totalReqTime/succes)
	}
}
