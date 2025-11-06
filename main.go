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
	"sync"

	amqp "github.com/rabbitmq/amqp091-go"
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
	queue *amqp.Channel
	qmx   *sync.Mutex
	qcond *sync.Cond
	pq    Queue
	vars  map[string]any
	funcs map[string]any
	kid   string
}

var mutex sync.Mutex

func NewHandler(queue *amqp.Channel, kid string) *handler {
	return &handler{
		queue: queue,
		pq:    make(Queue, 0),
		qmx:   &sync.Mutex{},
		qcond: sync.NewCond(&mutex),
		vars:  make(map[string]any, 10),
		funcs: make(map[string]any, 10),
		kid:   kid,
	}
}

func (hh *handler) Process(w http.ResponseWriter, r *http.Request) {
	blockID := r.URL.Query().Get("block_id")
	userID := r.URL.Query().Get("user_id")

	pluginName := "/noted/codes/" + hh.kid + "/" + userID + "/" + "block_" + blockID + ".so"
	fmt.Printf("plugin name: %s", pluginName)
	p, err := plugin.Open(pluginName)
	if err != nil {
		fmt.Fprintf(w, "Error opening plugin %s", err.Error())
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
		expectedName := "Export_block_" + msg.name
		v, err := msg.pq.Lookup(expectedName)
		if err != nil {
			message := KernelMessage{}
			message.KernelID = hh.kid
			message.BlockID = msg.name
			message.Fail = true
			message.Result = err.Error()
			bts, _ := json.Marshal(message)
			hh.queue.PublishWithContext(context.Background(),
				"",              // exchange
				"noted-kernels", // routing key
				false,           // mandatory
				false,           // immediate
				amqp.Publishing{
					ContentType: "application/json",
					Body:        bts,
				})
			continue
		}
		old := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Println("panic occured: ", r)

					message := KernelMessage{}
					message.KernelID = hh.kid
					message.BlockID = msg.name
					message.Fail = true
					message.Result = fmt.Sprintf("panic: %v", r)
					bts, _ := json.Marshal(message)
					_ = hh.queue.PublishWithContext(context.Background(),
						"",              // exchange
						"noted-kernels", // routing key
						false,           // mandatory
						false,           // immediate
						amqp.Publishing{
							ContentType: "application/json",
							Body:        bts,
						})
				}
			}()
			v.(func(*map[string]any, *map[string]any))(&hh.vars, &hh.funcs)
		}()
		outC := make(chan string)
		go func() {
			var buf bytes.Buffer
			io.Copy(&buf, r)
			outC <- buf.String()
		}()

		// back to normal state
		w.Close()
		os.Stdout = old // restoring the real stdout
		out := <-outC
		message := KernelMessage{}
		message.KernelID = hh.kid
		message.BlockID = msg.name
		message.Fail = false
		message.Result = out
		bts, _ := json.Marshal(message)
		hh.queue.PublishWithContext(context.Background(),
			"",              // exchange
			"noted-kernels", // routing key
			false,           // mandatory
			false,           // immediate
			amqp.Publishing{
				ContentType: "application/json",
				Body:        bts,
			})
	}
}

func main() {
	amqpAddr := os.Args[1] // "amqp://guest:guest@172.26.0.2:5672/"
	conn, err := amqp.Dial(amqpAddr)
	if err != nil {
		log.Fatalf("unable to open connect to RabbitMQ server. Error: %s", err)
	}

	defer func() {
		_ = conn.Close() // Закрываем подключение в случае удачной попытки подключения
	}()

	ch, err := conn.Channel()
	if err != nil {
		log.Fatalf("failed to open a channel. Error: %s", err)
	}

	defer func() {
		_ = ch.Close() // Закрываем подключение в случае удачной попытки подключения
	}()

	kernelId := os.Args[2]
	hh := NewHandler(ch, kernelId)

	http.HandleFunc("/run", hh.Process)

	go func() {
		hh.runQueue()
	}()

	// Start the HTTP server on port 8080
	fmt.Println("Server starting on port 8080...")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
