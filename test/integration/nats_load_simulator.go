// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/nats-io/nats.go"
)

// SS7 Gateway Simulator
// Simulates external SS7 network or ss7-ha-gateway for testing

var (
	natsURL     = flag.String("nats", "nats://localhost:4222", "NATS server URL")
	gatewayAPI  = flag.String("api", "http://localhost:8080", "Gateway API URL")
	mode        = flag.String("mode", "subscriber", "Mode: subscriber, publisher, or both")
	messageRate = flag.Int("rate", 10, "Messages per second")
	duration    = flag.Int("duration", 60, "Test duration in seconds")
)

// Message types matching the gateway
type MTSmsRequest struct {
	CorrelationID  string `json:"correlation_id"`
	MSISDN         string `json:"msisdn"`
	Sender         string `json:"sender"`
	Content        string `json:"content"`
	Encoding       string `json:"encoding"`
	ValidityPeriod int    `json:"validity_period"`
}

type MOSmsMessage struct {
	MessageID     string    `json:"message_id"`
	Sender        string    `json:"sender"`
	Recipient     string    `json:"recipient"`
	Content       string    `json:"content"`
	Timestamp     time.Time `json:"timestamp"`
	CorrelationID string    `json:"correlation_id,omitempty"`
}

type MTSmsResponse struct {
	CorrelationID string    `json:"correlation_id"`
	Status        string    `json:"status"`
	ErrorCode     int32     `json:"error_code"`
	Timestamp     time.Time `json:"timestamp"`
}

type DeliveryReport struct {
	MessageID string    `json:"message_id"`
	MSISDN    string    `json:"msisdn"`
	Status    string    `json:"status"`
	Timestamp time.Time `json:"timestamp"`
	ErrorCode int32     `json:"error_code"`
}

type Statistics struct {
	MessagesSent     int
	MessagesReceived int
	Errors           int
	StartTime        time.Time
	LastMessageTime  time.Time
}

func main() {
	flag.Parse()

	log.Printf("SS7 Gateway Simulator starting...")
	log.Printf("Mode: %s, Rate: %d msg/s, Duration: %d seconds", *mode, *messageRate, *duration)

	// Connect to NATS
	nc, err := nats.Connect(*natsURL,
		nats.Name("ss7-simulator"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		log.Fatalf("Failed to connect to NATS: %v", err)
	}
	defer nc.Close()

	log.Printf("✓ Connected to NATS: %s", nc.ConnectedUrl())

	// Check gateway health
	if err := checkGatewayHealth(); err != nil {
		log.Printf("⚠ Warning: Gateway health check failed: %v", err)
	} else {
		log.Printf("✓ Gateway is healthy")
	}

	stats := &Statistics{
		StartTime: time.Now(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*duration)*time.Second)
	defer cancel()

	// Start subscriber if needed
	if *mode == "subscriber" || *mode == "both" {
		go startSubscriber(nc, stats)
	}

	// Start publisher if needed
	if *mode == "publisher" || *mode == "both" {
		go startPublisher(ctx, nc, stats)
	}

	// Stats reporter
	go reportStats(ctx, stats)

	// Wait for duration
	<-ctx.Done()

	// Final stats
	time.Sleep(2 * time.Second) // Let last messages process
	printFinalStats(stats)
}

func startSubscriber(nc *nats.Conn, stats *Statistics) {
	log.Printf("Starting subscriber...")

	// Subscribe to MO-SMS
	nc.Subscribe("map.mo.sms", func(msg *nats.Msg) {
		var moSms MOSmsMessage
		if err := json.Unmarshal(msg.Data, &moSms); err != nil {
			log.Printf("Error parsing MO-SMS: %v", err)
			stats.Errors++
			return
		}

		stats.MessagesReceived++
		stats.LastMessageTime = time.Now()

		log.Printf("📱 Received MO-SMS: %s → %s: %s",
			moSms.Sender, moSms.Recipient, moSms.Content)

		// Send MT-SMS response
		response := MTSmsResponse{
			CorrelationID: moSms.CorrelationID,
			Status:        "DELIVERED",
			ErrorCode:     0,
			Timestamp:     time.Now(),
		}

		data, _ := json.Marshal(response)
		nc.Publish("map.mt.sms.response", data)
	})

	// Subscribe to MT-SMS responses
	nc.Subscribe("map.mt.sms.response", func(msg *nats.Msg) {
		var response MTSmsResponse
		if err := json.Unmarshal(msg.Data, &response); err != nil {
			log.Printf("Error parsing MT-SMS response: %v", err)
			stats.Errors++
			return
		}

		stats.MessagesReceived++
		stats.LastMessageTime = time.Now()

		log.Printf("✓ Received MT-SMS response: %s - %s",
			response.CorrelationID, response.Status)
	})

	// Subscribe to delivery reports
	nc.Subscribe("map.delivery.report", func(msg *nats.Msg) {
		var report DeliveryReport
		if err := json.Unmarshal(msg.Data, &report); err != nil {
			log.Printf("Error parsing delivery report: %v", err)
			stats.Errors++
			return
		}

		stats.MessagesReceived++
		stats.LastMessageTime = time.Now()

		log.Printf("📨 Delivery report: %s - %s", report.MessageID, report.Status)
	})

	// Subscribe to SRI responses
	nc.Subscribe("map.sri.response", func(msg *nats.Msg) {
		stats.MessagesReceived++
		stats.LastMessageTime = time.Now()
		log.Printf("🔍 Received SRI response")
	})

	// Subscribe to CAP events
	nc.Subscribe("cap.event.report", func(msg *nats.Msg) {
		stats.MessagesReceived++
		stats.LastMessageTime = time.Now()
		log.Printf("📞 Received CAP event report")
	})

	// Subscribe to INAP events
	nc.Subscribe("inap.event.report", func(msg *nats.Msg) {
		stats.MessagesReceived++
		stats.LastMessageTime = time.Now()
		log.Printf("🔔 Received INAP event report")
	})

	log.Printf("✓ Subscriber ready (6 topics)")
}

func startPublisher(ctx context.Context, nc *nats.Conn, stats *Statistics) {
	log.Printf("Starting publisher...")

	ticker := time.NewTicker(time.Second / time.Duration(*messageRate))
	defer ticker.Stop()

	messageNum := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			messageNum++

			// Publish MT-SMS request
			request := MTSmsRequest{
				CorrelationID:  fmt.Sprintf("test-%d-%d", time.Now().Unix(), messageNum),
				MSISDN:         fmt.Sprintf("+90555%07d", messageNum%10000000),
				Sender:         "+905559999999",
				Content:        fmt.Sprintf("Test message #%d from simulator", messageNum),
				Encoding:       "GSM7",
				ValidityPeriod: 3600,
			}

			data, err := json.Marshal(request)
			if err != nil {
				log.Printf("Error marshaling request: %v", err)
				stats.Errors++
				continue
			}

			if err := nc.Publish("map.mt.sms.request", data); err != nil {
				log.Printf("Error publishing: %v", err)
				stats.Errors++
				continue
			}

			stats.MessagesSent++
			stats.LastMessageTime = time.Now()

			if messageNum%100 == 0 {
				log.Printf("📤 Sent %d messages", messageNum)
			}
		}
	}
}

func reportStats(ctx context.Context, stats *Statistics) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			duration := time.Since(stats.StartTime)
			sendRate := float64(stats.MessagesSent) / duration.Seconds()
			recvRate := float64(stats.MessagesReceived) / duration.Seconds()

			log.Printf("📊 Stats: Sent=%d (%.1f/s), Received=%d (%.1f/s), Errors=%d",
				stats.MessagesSent, sendRate,
				stats.MessagesReceived, recvRate,
				stats.Errors)
		}
	}
}

func printFinalStats(stats *Statistics) {
	duration := time.Since(stats.StartTime)

	fmt.Println("\n========================================")
	fmt.Println("         SIMULATION COMPLETE")
	fmt.Println("========================================")
	fmt.Printf("Duration:         %.2f seconds\n", duration.Seconds())
	fmt.Printf("Messages Sent:    %d (%.2f msg/s)\n",
		stats.MessagesSent, float64(stats.MessagesSent)/duration.Seconds())
	fmt.Printf("Messages Received: %d (%.2f msg/s)\n",
		stats.MessagesReceived, float64(stats.MessagesReceived)/duration.Seconds())
	fmt.Printf("Errors:           %d\n", stats.Errors)
	fmt.Printf("Success Rate:     %.2f%%\n",
		float64(stats.MessagesReceived)/float64(stats.MessagesSent)*100)
	fmt.Println("========================================")
}

func checkGatewayHealth() error {
	resp, err := http.Get(*gatewayAPI + "/api/v1/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gateway returned status %d", resp.StatusCode)
	}

	return nil
}
