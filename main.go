package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	"github.com/Shopify/sarama"
	alertmanager "github.com/prometheus/alertmanager/template"
)

var (
	port             int
	interval         int
	timeout          int
	smtpSmarthost    string
	smtpAuthUsername string
	smtpAuthPassword string
	smtpFrom         string
	smtpTo           string
	kafkaAddrs       string
	kafkaTopic       string
	maxRetry         int
)

func init() {
	flag.IntVar(&port, "port", 9087, "listen port")
	flag.IntVar(&interval, "interval", 30, "check interval in seconds")
	flag.IntVar(&timeout, "timeout", 3, "timeout in minutes")
	flag.IntVar(&maxRetry, "max-retry", 3, "max retry to connect kafka")
	flag.StringVar(&kafkaAddrs, "kafka-addrs", "", "comma separated kafka addresses")
	flag.StringVar(&kafkaTopic, "kafka-topic", "", "Kafka topic")
	flag.StringVar(&smtpSmarthost, "smtp-smarthost", "", "SMTP Server")
	flag.StringVar(&smtpAuthUsername, "smtp-auth-username", "", "SMTP Auth Username")
	flag.StringVar(&smtpAuthPassword, "smtp-auth-password", "", "SMTP Auth Password")
	flag.StringVar(&smtpFrom, "smtp-from", "", "SMTP From")
	flag.StringVar(&smtpTo, "smtp-to", "", "SMTP To")
	flag.Parse()
}

func main() {
	var sender Sender
	if len(kafkaAddrs) != 0 {
		addrs := strings.Split(kafkaAddrs, ",")
		sender = newSender(addrs)
	}

	ch := make(chan time.Time, 5)
	http.HandleFunc("/alert", func(w http.ResponseWriter, req *http.Request) {
		defer req.Body.Close()

		data := alertmanager.Data{}
		if err := json.NewDecoder(req.Body).Decode(&data); err != nil {
			errorHandler(w, http.StatusBadRequest, err, &data)
			return
		}
		log.Printf("received data: %v", data)

		for _, alert := range data.Alerts.Firing() {
			if alertname, ok := alert.Labels["alertname"]; ok && alertname == "Watchdog" {
				select {
				case ch <- time.Now():
					log.Printf("Alert fired")
				default:
					log.Printf("channel blocked")
				}
				ch <- time.Now()
			}
		}
	})
	http.HandleFunc("/health", func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte("OK"))
	})

	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()
	log.Printf("Ticker begin")
	last := time.Now()
	go func() {
		for t := range ticker.C {
			log.Print("tick at ", t)
			select {
			case last = <-ch:
				log.Printf("Received timestamp from channel")
			default:
				log.Printf("Channel empty")
			}
			log.Printf("last timestamp: %v, now: %v, timeout: %v", last, time.Now(), time.Duration(timeout)*time.Minute)
			if last.Add(time.Duration(timeout) * time.Minute).Before(time.Now()) {
				log.Printf("Alert dead")
				sender.Send(fmt.Sprintf("Alert pipeline is broken since %v", last))
			}
		}
	}()

	log.Printf("Listening on %d", port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", port), nil))
}

type loginAuth struct {
	username, password string
}

// LoginAuth ... implements stmp.Auth
func LoginAuth(username, password string) smtp.Auth {
	return &loginAuth{username, password}
}

func (a loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", []byte{}, nil
}

// Used for AUTH LOGIN. (Maybe password should be encrypted)
func (a loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		switch strings.ToLower(string(fromServer)) {
		case "username:":
			return []byte(a.username), nil
		case "password:":
			return []byte(a.password), nil
		default:
			return nil, fmt.Errorf("unexpected server challenge")
		}
	}
	return nil, nil
}

//KafkaMsg represents kafka message
type KafkaMsg struct {
	Title       string `json:"event_object"`
	Source      string `json:"object_name"`
	Instance    string `json:"object_ip"`
	Description string `json:"event_msg"`
	Time        string `json:"event_time"`
	Level       string `json:"event_level"`
	Summary     string `json:"summary"`
	Expr        string `json:"expr"`
	Value       string `json:"value"`
	URL         string `json:"url"`
}

type Sender struct {
	kafkaClient sarama.SyncProducer
	mailClient  *smtp.Client
}

func newSender(kafkaAddrs []string) Sender {
	var s Sender
	var err error
	s.kafkaClient, err = newKafkaClient(kafkaAddrs)
	if err != nil {
		log.Printf("failed to create kafka client")
	}

	s.mailClient, err = newMailClient()
	if err != nil {
		log.Printf("failed to create email client")
	}
	return s
}

func (s Sender) Send(msg string) error {
	if s.kafkaClient != nil {
		// send kafka
		kafkaMsg := &sarama.ProducerMessage{
			Topic: kafkaTopic,
			Value: sarama.StringEncoder(msg),
		}

		partition, offset, err := s.kafkaClient.SendMessage(kafkaMsg)
		if err != nil {
			return err
		}
		log.Printf("Produced message %s to kafka cluster partition %d with offset %d", msg, partition, offset)
	}

	if s.mailClient != nil {
		// send email
		if err := s.mailClient.Mail(smtpFrom); err != nil {
			log.Printf("failed to issue MAIL command: %v", err)
			return err
		}
		addrs, err := mail.ParseAddressList(smtpTo)
		if err != nil {
			log.Printf("failed to parse address list %s: %v", smtpTo, err)
			return err
		}
		for _, addr := range addrs {
			if err := s.mailClient.Rcpt(addr.Address); err != nil {
				log.Printf("failed to issue RCPT command for %s: %v", addr.Address, err)
				return err
			}
		}
		mailMsg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s",
			smtpFrom, smtpTo, "[Firing] Alerting pipeline is broken", msg,
		)
		writer, err := s.mailClient.Data()
		if err != nil {
			log.Printf("failed to issue DATA command: %v", err)
			return err
		}
		defer writer.Close()

		buf := bytes.NewBufferString(mailMsg)
		if _, err := buf.WriteTo(writer); err != nil {
			log.Printf("failed to write buffer: %v", err)
			return err
		}
		log.Printf("Send alert mail %s to smtp server", mailMsg)
	}

	return nil
}

func newKafkaClient(addrs []string) (sarama.SyncProducer, error) {
	var client sarama.SyncProducer
	var err error
	for i := 0; i < maxRetry; i++ {
		addrs := strings.Split(kafkaAddrs, ",")
		config := sarama.NewConfig()
		config.Producer.Return.Successes = true
		config.Producer.RequiredAcks = sarama.WaitForAll

		client, err = sarama.NewSyncProducer(addrs, config)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		return client, nil
	}
	return nil, err
}

func newMailClient() (*smtp.Client, error) {
	client, err := smtp.Dial(smtpSmarthost)
	if err != nil {
		log.Printf("failed to dial mail server: %v", err)
		return nil, err
	}
	if ok, _ := client.Extension("AUTH"); ok {
		auth := loginAuth{smtpAuthUsername, smtpAuthPassword}
		if err := client.Auth(auth); err != nil {
			log.Printf("failed to auth: %v", err)
			return nil, err
		}
	}

	return client, nil
}

func errorHandler(w http.ResponseWriter, status int, err error, data *alertmanager.Data) {
	w.WriteHeader(status)

	response := struct {
		Error   bool
		Status  int
		Message string
	}{
		true,
		status,
		err.Error(),
	}
	// JSON response
	bytes, err := json.Marshal(response)
	if err != nil {
		log.Printf("%v", err)
		return
	}
	json := string(bytes[:])
	fmt.Fprint(w, json)

	log.Printf("%d %s: err=%s groupLabels=%+v", status, http.StatusText(status), err, data.GroupLabels)
}
