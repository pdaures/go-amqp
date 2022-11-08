package amqp_test

import (
	"context"
	"fmt"
	"log"
	"time"

	amqp "github.com/pdaures/go-amqp"
)

func Example() {
	// Create client
	client, err := amqp.Dial("amqps://my-namespace.servicebus.windows.net", &amqp.ConnOptions{
		SASLType: amqp.SASLTypePlain("access-key-name", "access-key"),
	})
	if err != nil {
		log.Fatal("Dialing AMQP server:", err)
	}
	defer client.Close()

	ctx := context.TODO()

	// Open a session
	session, err := client.NewSession(ctx, nil)
	if err != nil {
		log.Fatal("Creating AMQP session:", err)
	}

	// Send a message
	{
		// Create a sender
		sender, err := session.NewSender(ctx, "/queue-name", nil)
		if err != nil {
			log.Fatal("Creating sender link:", err)
		}

		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)

		// Send message
		err = sender.Send(ctx, amqp.NewMessage([]byte("Hello!")))
		if err != nil {
			log.Fatal("Sending message:", err)
		}

		sender.Close(ctx)
		cancel()
	}

	// Continuously read messages
	{
		// Create a receiver
		receiver, err := session.NewReceiver(ctx, "/queue-name", &amqp.ReceiverOptions{
			Credit: 10,
		})
		if err != nil {
			log.Fatal("Creating receiver link:", err)
		}
		defer func() {
			ctx, cancel := context.WithTimeout(ctx, 1*time.Second)
			receiver.Close(ctx)
			cancel()
		}()

		for {
			// Receive next message
			msg, err := receiver.Receive(ctx)
			if err != nil {
				log.Fatal("Reading message from AMQP:", err)
			}

			// Accept message
			if err = receiver.AcceptMessage(context.TODO(), msg); err != nil {
				log.Fatalf("Failure accepting message: %v", err)
			}

			fmt.Printf("Message received: %s\n", msg.GetData())
		}
	}
}
