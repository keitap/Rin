package rin

import (
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/crowdmob/goamz/aws"
	"github.com/crowdmob/goamz/sqs"
)

var SQS *sqs.SQS
var config *Config
var Debug bool
var Runnable bool

var TrapSignals = []os.Signal{
	syscall.SIGHUP,
	syscall.SIGINT,
	syscall.SIGTERM,
	syscall.SIGQUIT,
}

func Run(configFile string) error {
	Runnable = true
	var err error
	log.Println("Loading config", configFile)
	config, err = LoadConfig(configFile)
	if err != nil {
		return err
	}
	for _, target := range config.Targets {
		log.Println("Define target", target)
	}

	auth := aws.Auth{
		AccessKey: config.Credentials.AWS_ACCESS_KEY_ID,
		SecretKey: config.Credentials.AWS_SECRET_ACCESS_KEY,
	}
	region := aws.GetRegion(config.Credentials.AWS_REGION)
	SQS = sqs.New(auth, region)

	shutdownCh := make(chan interface{})
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, TrapSignals...)

	// run worker
	var wg sync.WaitGroup
	wg.Add(1)
	go sqsWorker(&wg, shutdownCh)

	// wait for signal
	s := <-signalCh
	switch sig := s.(type) {
	case syscall.Signal:
		log.Printf("Got signal: %s(%d)", sig, sig)
	default:
	}
	log.Println("Shutting down worker...")
	close(shutdownCh) // notify shutdown to worker

	wg.Wait() // wait for worker completed
	log.Println("Shutdown successfully")
	return nil
}

func waitForRetry() {
	log.Println("Retry after 10 sec.")
	time.Sleep(10 * time.Second)
}

func runnable(ch chan interface{}) bool {
	if !Runnable {
		return false
	}
	select {
	case <-ch:
		// ch closed == shutdown
		Runnable = false
		return false
	default:
	}
	return true
}

func sqsWorker(wg *sync.WaitGroup, ch chan interface{}) {
	defer (*wg).Done()

	log.Printf("Starting up SQS Worker")
	defer log.Println("Shutdown SQS Worker")

	for runnable(ch) {
		log.Println("Connect to SQS:", config.QueueName)
		queue, err := SQS.GetQueue(config.QueueName)
		if err != nil {
			log.Println("Can't get queue:", err)
			waitForRetry()
			continue
		}
		quit, err := handleQueue(queue, ch)
		if err != nil {
			log.Println("Processing failed:", err)
			waitForRetry()
			continue
		}
		if quit {
			break
		}
	}
}

func handleQueue(queue *sqs.Queue, ch chan interface{}) (bool, error) {
	for runnable(ch) {
		err := handleMessage(queue)
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

func handleMessage(queue *sqs.Queue) error {
	res, err := queue.ReceiveMessage(1)
	if err != nil {
		return err
	}
	if len(res.Messages) == 0 {
		return nil
	}
	msg := res.Messages[0]
	log.Printf("Starting process message id:%s handle:%s", msg.MessageId, msg.ReceiptHandle)
	if Debug {
		log.Println("message body:", msg.Body)
	}
	event, err := ParseEvent([]byte(msg.Body))
	if err != nil {
		log.Println("Can't parse event from Body.", err)
		return err
	}
	log.Println("Importing event:", event)
	n, err := Import(event)
	if err != nil {
		log.Println("Import failed.", err)
		return err
	}
	if n == 0 {
		log.Println("All events were not matched for any targets. Ignored.")
	} else {
		log.Printf("%d import action completed.", n)
	}
	_, err = queue.DeleteMessage(&msg)
	if err != nil {
		log.Println("Can't delete message.", err)
	}
	log.Printf("Completed message ID:%s", msg.MessageId)
	return nil
}
