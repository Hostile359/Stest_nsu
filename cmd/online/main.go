package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"

	"math/rand"
	"os"

	"sync"
	"sync/atomic"

	"time"

	"github.com/aybabtme/uniplot/histogram"
	"github.com/montanaflynn/stats"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

var wsErors int64 = 0 // Суммарное количетво ошибок вебсокета
var sampleRate = 48000
var rescore = false
var duration int // Продолжительность работы теста в минутах
var pauseMin = 0 // Нижняя граница для рандома паузы между запросами
var pauseMax = 0 // Верхняя граница для рандома паузы между запросами

type audioInfo struct {
	filename string
	audio    []byte
}

type AudioJson struct {
	AsrRes  string  `json:"asr_result"`
	Command string  `json:"command"`
	RescRes string  `json:"rescoring_result"`
	Status  string  `json:"status"`
	Time    float32 `json:"time"`
}

type AudioRes struct {
	AudioJson
	Filename string `json:"filename"`
}

func readFile(filename string) ([]byte, error) {
	var audio []byte
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := make([]byte, 1024)
	for {
		n, err := f.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		audio = append(audio, buf[:n]...)
	}
	return audio, nil
}

// Вывод сообщения воркера
func workerPrint(msg string, workerNum int) {
	fmt.Printf("Worker: %d, Msg: %s\n", workerNum, msg)
}

func timer(sendChan chan int, recvChan chan int, timeChan chan time.Duration, stepCount int) {
	for i := 1; i <= stepCount; i++ {
		time.Sleep(time.Duration(250) * time.Millisecond)
		sendChan <- i // Отпрвка сигнала для поссылки чанка
	}
	close(sendChan)

	start := time.Now()

	<-recvChan // Получение сигнала о завершении

	timeChan <- time.Since(start) // Отправка итогового времени
	close(timeChan)
}

// Отправка pcm аудиофайла на сервер
func sendPcm(audio []byte, host string, workerNum int) (*AudioJson, time.Duration, error) {
	ctx := context.Background()

	bufSize := int(float64(sampleRate*2) * 0.25)
	if len(audio)%bufSize != 0 { // длина аудио должна быть кратна bufSize
		return nil, 0, fmt.Errorf("wrong audio len %d %% %d != 0", len(audio), bufSize)
	}
	stepCount := len(audio) / bufSize
	recvChan := make(chan int, stepCount) // канал для получения сигналов на поссылку чанка
	sendChan := make(chan int)            // канал для отправки сигнала о завершении распознования
	timeChan := make(chan time.Duration)

	go timer(recvChan, sendChan, timeChan, stepCount)

	conn, _, err := websocket.Dial(ctx, host, nil)
	if err != nil {
		workerPrint(err.Error(), workerNum)
		return nil, time.Duration(0), err
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
		return nil, time.Duration(0), err
	}

	reader := bytes.NewReader(audio)

	buf := make([]byte, bufSize) // буфер равный 250 мс
	var respJson AudioJson

	for {
		n, err := reader.Read(buf)
		if err != nil {
			close(sendChan)
			return nil, time.Duration(0), err
		}

		i := <-recvChan

		err = conn.Write(ctx, websocket.MessageBinary, buf[:n])
		if err != nil {
			workerPrint(err.Error(), workerNum)
			close(sendChan)
			return nil, time.Duration(0), err
		}

		err = wsjson.Read(ctx, conn, &respJson)
		if err != nil {
			workerPrint(err.Error(), workerNum)
			close(sendChan)
			return nil, time.Duration(0), err
		}

		// workerPrint(fmt.Sprintf("%d/%d: %#v", i, stepCount, respJson), workerNum)

		if i == stepCount {
			if respJson.Status == "result" {
				sendChan <- 0
				close(sendChan)
				break
			}

			err = conn.Write(ctx, websocket.MessageBinary, buf[:n])
			if err != nil {
				workerPrint(err.Error(), workerNum)
				close(sendChan)
				return nil, time.Duration(0), err
			}

			err = wsjson.Read(ctx, conn, &respJson)
			if err != nil {
				workerPrint(err.Error(), workerNum)
				close(sendChan)
				return nil, time.Duration(0), err
			}

			sendChan <- 0
			close(sendChan)
			break
		}
	}
	conn.Close(websocket.StatusNormalClosure, "")

	elapsedTime := <-timeChan

	return &respJson, elapsedTime, nil
}

// Запуск воркера, итерантивно отправляющего случайное аудио из списка на распознование в течении duration минут.
// Каждый последующий запрос отправляется с задержкой 0-50ms
func workerProc(audios []audioInfo, host string, workerNum int, 
				wg *sync.WaitGroup, timeChan chan []int64, resChan chan []AudioRes,
				m *metrics) {
	defer wg.Done()

	timesList := make([]int64, 0, duration*60)
	resList := make([]AudioRes, 0, duration*60)

	end := time.Now().Add(time.Duration(duration) * time.Minute)
	runLoop := true
	for runLoop {
		if time.Now().After(end) {
			runLoop = false
		}

		time.Sleep(time.Duration(rand.Intn(pauseMax)+pauseMin) * time.Millisecond)
		audioIndex := rand.Intn(len(audios))
		if respJson, elapsedTime, err := sendPcm(audios[audioIndex].audio, host, workerNum); err == nil {
			respText := fmt.Sprintf("%#v, %dms", respJson, elapsedTime.Milliseconds())
			workerPrint(respText, workerNum)

			timesList = append(timesList, elapsedTime.Milliseconds())

			m.duration.With(prometheus.Labels{}).Observe(elapsedTime.Seconds())

			res := AudioRes{
				Filename:  audios[audioIndex].filename,
				AudioJson: *respJson,
			}
			resList = append(resList, res)
		} else {
			workerPrint(err.Error(), workerNum)
			atomic.AddInt64(&wsErors, 1)
		}
	}
	timeChan <- timesList
	resChan <- resList
}

func main() {
	rand.Seed(time.Now().UnixNano())
	var wg sync.WaitGroup
	var pcmPath, host string
	var numWorkers int
	var csvFilename string
	var avgWsErrors int64
	var bins, width int
	var name string
	var plotFilename string
	var resFilename string

	flag.StringVar(&pcmPath, "pcmpath", "", "Path to dir with pcm files")
	flag.StringVar(&host, "host", "", "Host adreess with port (e.g. localhost:2700)")
	flag.IntVar(&numWorkers, "worker", 1, "Workers count")
	flag.IntVar(&duration, "duration", 30, "Test duration in mins")
	flag.IntVar(&sampleRate, "sr", 48000, "Samplerate")
	flag.BoolVar(&rescore, "rescore", false, "Use rescore")
	flag.IntVar(&pauseMin, "pause_min", 1, "Low border of random for pause duration in ms")
	flag.IntVar(&pauseMax, "pause_max", 50, "High border of random for pause duration in ms")
	flag.StringVar(&csvFilename, "csv", "", "Path to output csv file (creates new or append string to existing one)")
	flag.IntVar(&bins, "bins", 9, "Hitogram bins")
	flag.IntVar(&width, "width", 5, "Hitogram width")
	flag.StringVar(&name, "run_name", "", "Name of the test run(if empty, it will be the same as filename)")
	flag.StringVar(&plotFilename, "plt", "", "Path to file for plot histogram(if empty, it will be ploted at stdout)")
	flag.StringVar(&resFilename, "res_file", "res.json", "Path to output json file with asr results")
	flag.Parse()

	if name == "" {
		name = pcmPath
	}

	timeChan := make(chan []int64, numWorkers)
	resChan := make(chan []AudioRes, numWorkers)

	fmt.Println(pcmPath, host, numWorkers, sampleRate, rescore)
	wsAsrHost := fmt.Sprintf("ws://%s", host)
	files, err := os.ReadDir(pcmPath)
	if err != nil {
		log.Fatal(err)
	}
	audios := make([]audioInfo, 0)
	for _, f := range files {
		fname := fmt.Sprint(pcmPath, "/", f.Name())
		fmt.Println(fname)
		audio_i, err := readFile(fname)
		if err != nil {
			log.Fatal(err)
		}
		audios = append(audios, audioInfo{filename: f.Name(), audio: audio_i})
	}

	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	stopServer := make(chan struct{}, 1)
	go startPromApi(stopServer, reg)
	defer func () {
		stopServer <- struct{}{}
	} ()


	log.Println("Start testing")
	for wN := 1; wN <= numWorkers; wN++ {
		log.Printf("Start worker: %d\n", wN)
		wg.Add(1)
		go workerProc(audios, wsAsrHost, wN, &wg, timeChan, resChan, m)
	}
	wg.Wait()
	close(timeChan)
	close(resChan)

	fullTimesList := make([]int64, 0, duration*numWorkers*60)
	for timesList := range timeChan {
		fullTimesList = append(fullTimesList, timesList...)
	}

	fullResList := make([]AudioRes, 0, duration*numWorkers*60)
	for resList := range resChan {
		fullResList = append(fullResList, resList...)
	}
	resJson, err := json.MarshalIndent(fullResList, "", "\t")
	if err != nil {
		log.Fatal(err)
	}
	resF, err := os.Create(resFilename)
	if err != nil {
		log.Fatal(err)
	}
	defer resF.Close()
	if _, err := resF.Write(resJson); err != nil {
		log.Fatal(err)
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
		writeCsvString := fmt.Sprintf("%s;%s;%d;%.0fms;%.0fms;%.0fms;%.0fms;%d;%dmin;%d\n", name, host, numWorkers, avgTime, medTime, maxTime, minTime, reqCount, duration, avgWsErrors)

		if _, err := os.Stat(csvFilename); err != nil {
			if os.IsNotExist(err) {
				log.Print("Create new file")
				writeCsvString = fmt.Sprint("name;host;workers;avg;median;max;min;reqs;duration;avgwserrors\n", writeCsvString)
			} else {
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

	var plotWriter io.Writer

	if plotFilename != "" {
		f, err := os.Create(plotFilename)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		plotWriter = f
		log.Printf("Write plt to %s\n", plotFilename)
	} else {
		plotWriter = os.Stdout
	}

	h := histogram.Hist(bins, data)
	err = histogram.Fprintf(plotWriter, h, histogram.Linear(width), func(v float64) string {
		return fmt.Sprintf("%dms", int(v)) //time.Duration(v).Milliseconds()
	})
	if err != nil {
		log.Fatal(err)
	}
}

func startPromApi(stopServer chan struct{}, reg *prometheus.Registry) {
	pMux := http.NewServeMux()
	promHandler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	pMux.Handle("/metrics", promHandler)

	srv := &http.Server{
		Addr: ":8081",
		Handler: pMux,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	<-stopServer
	
	if err := srv.Shutdown(context.Background()); err != nil {
		log.Fatal(err)
	}
}
