package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"golang.org/x/time/rate"
)

const SenderLen = 3

const (
	RateLimitRate  = 20
	RateLimitBurst = 10
	MaxFilterLen   = 50
)

func HandleWebsocket(ctx context.Context, req *http.Request, connID string, conn net.Conn, router *Router, db *DB) error {
	defer func() {
		if err := recover(); err != nil {
			logStderr.Printf("[%v, %v]: paniced: %v", req.RemoteAddr, connID, err)
			panic(err)
		}
	}()

	defer router.Delete(connID)

	sender := make(chan ServerMsg, SenderLen)

	errCh := make(chan error, 2)

	wg := new(sync.WaitGroup)
	defer wg.Wait()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- wsSender(ctx, req, connID, conn, router, sender)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- wsReceiver(ctx, req, connID, conn, router, db, sender)
	}()

	err := <-errCh

	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("handle websocket error: %w", err)
	}

	return nil
}

func wsReceiver(
	ctx context.Context,
	req *http.Request,
	connID string,
	conn net.Conn,
	router *Router,
	db *DB,
	sender chan<- ServerMsg,
) error {
	lim := rate.NewLimiter(RateLimitRate, RateLimitBurst)
	reader := wsutil.NewServerSideReader(conn)

	for {
		if err := lim.Wait(ctx); err != nil {
			return fmt.Errorf("rate limiter returns error: %w", err)
		}

		payload, err := wsRead(reader)
		if err != nil {
			return fmt.Errorf("receive error: %w", err)
		}

		if !utf8.Valid(payload) {
			logStderr.Printf("[%v, %v]: payload is not utf8: %v", req.RemoteAddr, connID, payload)
			continue
		}

		strMsg := string(payload)
		jsonMsg, err := ParseClientMsgJSON(strMsg)
		if err != nil {
			logStderr.Printf("[%v, %v]: received invalid msg: %v", req.RemoteAddr, connID, err)
			continue
		}

		DoAccessLog(req.RemoteAddr, connID, AccessLogRecv, strMsg)

		switch msg := jsonMsg.(type) {
		case *ClientReqMsgJSON:
			if err := serveClientReqMsgJSON(connID, router, db, sender, msg); err != nil {
				logStderr.Printf("[%v, %v]: failed to serve client req msg %v", req.RemoteAddr, connID, err)
				continue
			}

		case *ClientCloseMsgJSON:
			if err := serveClientCloseMsgJSON(connID, router, msg); err != nil {
				logStderr.Printf("[%v, %v]: failed to serve client close msg %v", req.RemoteAddr, connID, err)
				continue
			}

		case *ClientEventMsgJSON:
			if err := serveClientEventMsgJSON(router, db, msg); err != nil {
				logStderr.Printf("[%v, %v]: failed to serve client event msg %v", req.RemoteAddr, connID, err)
				continue
			}
		}
	}
}

func wsRead(wsr *wsutil.Reader) ([]byte, error) {
	limit := *MaxClientMesLen + 1

	hdr, err := wsr.NextFrame()
	if err != nil {
		return nil, fmt.Errorf("failed to get next frame: %w", err)
	}
	if hdr.OpCode == ws.OpClose {
		return nil, io.EOF
	}

	r := io.LimitReader(wsr, int64(limit))
	res, err := io.ReadAll(r)
	if len(res) == limit {
		return res, fmt.Errorf("websocket message is too long (len=%v): %s", len(res), res)
	}
	return res, err
}

func serveClientReqMsgJSON(
	connID string,
	router *Router,
	db *DB,
	sender chan<- ServerMsg,
	msg *ClientReqMsgJSON,
) error {
	filters := NewFiltersFromFilterJSONs(msg.FilterJSONs)

	if len(filters) > MaxFilterLen+2 {
		return fmt.Errorf("filter is too long: %v", msg)
	}

	for _, event := range db.FindAll(filters) {
		sender <- &ServerEventMsg{msg.SubscriptionID, event.EventJSON}
	}
	sender <- &ServerEOSEMsg{msg.SubscriptionID}

	router.Subscribe(connID, msg.SubscriptionID, filters, sender)
	return nil
}

func serveClientCloseMsgJSON(connID string, router *Router, msg *ClientCloseMsgJSON) error {
	if err := router.Close(connID, msg.SubscriptionID); err != nil {
		return fmt.Errorf("cannot close conn %v", msg.SubscriptionID)
	}
	return nil
}

func serveClientEventMsgJSON(router *Router, db *DB, msg *ClientEventMsgJSON) error {
	ok, err := msg.EventJSON.Verify()
	if err != nil {
		return fmt.Errorf("failed to verify event json: %v", msg)

	}
	if !ok {
		return fmt.Errorf("invalid signature: %v", msg)
	}

	event := &Event{msg.EventJSON, time.Now()}

	db.Save(event)

	if err := router.Publish(event); err != nil {
		return fmt.Errorf("failed to publish event: %v", event)
	}
	return nil
}

func wsSender(
	ctx context.Context,
	req *http.Request,
	connID string,
	conn net.Conn,
	router *Router,
	sender <-chan ServerMsg,
) (err error) {
	defer func() {
		if _, e := conn.Write(ws.CompiledCloseNormalClosure); e != nil {
			if errors.Is(e, net.ErrClosed) {
				return
			}
			err = fmt.Errorf("failed to send close frame: %w", e)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil

		case msg := <-sender:
			jsonMsg, err := msg.MarshalJSON()
			if err != nil {
				logStderr.Printf("[%v, %v]: failed to marshal server msg: %v", req.RemoteAddr, connID, msg)
			}

			if err := wsutil.WriteServerText(conn, jsonMsg); err != nil {
				if errors.Is(err, net.ErrClosed) {
					return nil
				}
				return fmt.Errorf("failed to write server text: %w", err)
			}

			DoAccessLog(req.RemoteAddr, connID, AccessLogSend, string(jsonMsg))
		}
	}
}
