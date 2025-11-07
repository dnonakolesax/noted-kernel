package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"plugin"
	"strconv"
	"sync"
	"time"

	"github.com/dnonakolesax/noted-kernel/rabbit"
)

type pluginMessage struct {
	pq   *plugin.Plugin
	name string
}

type Queue []pluginMessage

func (q *Queue) Push(element pluginMessage) {
	*q = append(*q, element)
}

func (q *Queue) Pop() (pluginMessage, bool) {
	if len(*q) == 0 {
		return pluginMessage{}, false // Queue is empty
	}
	element := (*q)[0]
	*q = (*q)[1:]
	return element, true
}

type handler struct {
	queue        *rabbit.RabbitProducer
	qmx          *sync.Mutex
	qcond        *sync.Cond
	pq           Queue
	vars         map[string]any
	funcs        map[string]any
	kid          string
	mountPath    string
	exportPrefix string
	blockTimeout time.Duration
	blockPrefix  string
}

var mutex sync.Mutex

func NewHandler(queue *rabbit.RabbitProducer, kid string, mountPath string, exportPrefix string, blockTimeout int, blockPrefix string) *handler {
	return &handler{
		queue:        queue,
		pq:           make(Queue, 0),
		qmx:          &sync.Mutex{},
		qcond:        sync.NewCond(&mutex),
		vars:         make(map[string]any, 10),
		funcs:        make(map[string]any, 10),
		kid:          kid,
		mountPath:    mountPath,
		blockTimeout: time.Duration(blockTimeout),
		blockPrefix:  blockPrefix,
		exportPrefix: exportPrefix,
	}
}

func (hh *handler) Process(w http.ResponseWriter, r *http.Request) {
	blockID := r.URL.Query().Get("block_id")
	userID := r.URL.Query().Get("user_id")

	pluginName := hh.mountPath + "/" + hh.kid + "/" + userID + "/" + hh.blockPrefix + blockID + ".so"

	defer func() {
		if r := recover(); r != nil {
			fmt.Println("panic occured: ", r)
			w.WriteHeader(http.StatusBadRequest)
		}
	}()

	p, err := plugin.Open(pluginName)
	if err != nil {
		fmt.Fprintf(w, "Error opening plugin %s, path: %s", err.Error(), pluginName)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	hh.qmx.Lock()
	hh.pq = append(hh.pq, pluginMessage{pq: p, name: blockID})
	hh.qmx.Unlock()
	mutex.Lock()
	hh.qcond.Broadcast()
	mutex.Unlock()
	w.WriteHeader(http.StatusOK)
}

type KernelMessage struct {
	KernelID string `json:"kernel_id"`
	BlockID  string `json:"block_id"`
	Result   string `json:"result"`
	Fail     bool   `json:"fail"`
}

func (hh *handler) runQueue() {
	for {
		hh.qmx.Lock()
		msg, ok := hh.pq.Pop()
		hh.qmx.Unlock()
		if !ok {
			mutex.Lock()
			hh.qcond.Wait()
			mutex.Unlock()
			continue
		}
		expectedName := hh.exportPrefix + msg.name
		v, err := msg.pq.Lookup(expectedName)
		if err != nil {
			fmt.Printf("error looking for %s", expectedName)
			message := KernelMessage{KernelID: hh.kid, BlockID: msg.name, Fail: true, Result: fmt.Sprintf("error: %s", err.Error())}
			bts, _ := json.Marshal(message)
			err := hh.queue.SendBytesWithRetry(context.Background(), bts)
			if err != nil {
				panic(err)
			}
			continue
		}
		old := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w
		tout := false
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Println("panic occured: ", r)

					message := KernelMessage{KernelID: hh.kid, BlockID: msg.name, Fail: true, Result: fmt.Sprintf("panic: %v", r)}
					bts, _ := json.Marshal(message)
					err := hh.queue.SendBytesWithRetry(context.Background(), bts)
					if err != nil {
						panic(err)
					}
				}
			}()
			tt := time.NewTimer(time.Second * hh.blockTimeout)
			doneChan := make(chan any)

			go func() {
				v.(func(*map[string]any, *map[string]any))(&hh.vars, &hh.funcs)
				doneChan <- struct{}{}
			}()
			select {
			case <-tt.C:
				tout = true
			case <-doneChan:
			}
		}()
		outC := make(chan string)
		go func() {
			var buf bytes.Buffer
			_, _ = io.Copy(&buf, r)
			outC <- buf.String()
		}()
		w.Close()
		os.Stdout = old
		out := <-outC
		message := KernelMessage{KernelID: hh.kid, BlockID: msg.name, Fail: tout, Result: out}
		if tout {
			message.Result = fmt.Sprintf("error: timeout %d seconds exceeded\n", hh.blockTimeout) + message.Result
		}
		bts, _ := json.Marshal(message)
		err = hh.queue.SendBytesWithRetry(context.Background(), bts)
		if err != nil {
			panic(err)
		}
	}
}

func main() {
	amqpAddr := os.Getenv("RMQ_ADDR")
	queueName := os.Getenv("CHAN_NAME")
	rConfig := rabbit.RabbitMQConfig{
		URL:        amqpAddr,
		Queue:      queueName,
		MaxRetries: 5,
		BaseDelay:  1 * time.Second,
		MaxDelay:   15 * time.Second,
	}

	r, err := rabbit.NewMessageSender(rConfig)

	if err != nil {
		panic(err)
	}

	defer func() {
		_ = r.Close()
	}()

	kernelId := os.Getenv("KERNEL_ID")
	mountPath := os.Getenv("MOUNT_PATH")
	exportPrefix := os.Getenv("EXPORT_PREFIX")
	blockPrefix := os.Getenv("BLOCK_PREFIX")
	toutStr := os.Getenv("TIMEOUT")
	tout, _ := strconv.Atoi(toutStr)
	hh := NewHandler(r, kernelId, mountPath, exportPrefix, tout, blockPrefix)

	http.HandleFunc("/run", hh.Process)
	http.HandleFunc("/alive", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	go func() {
		hh.runQueue()
	}()

	fmt.Println("Server starting on port 8080...")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
