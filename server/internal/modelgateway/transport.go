package modelgateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

type upstreamTransport struct {
	execution *execution
	next      http.RoundTripper
}

func (t upstreamTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	e := t.execution
	if e.attempt > 0 {
		tx, err := e.guard(r.Context())
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(context.Background())
		begin := e.beginRequest()
		var until time.Time
		if err = tx.QueryRow(r.Context(), `SELECT lease_until FROM learning_model_execution_leases WHERE id=$1`, e.lease.ID).Scan(&until); err != nil {
			return nil, err
		}
		begin.LeaseUntil = &until
		ref, err := e.service.Usage.BeginTx(r.Context(), tx, begin)
		if err != nil {
			return nil, err
		}
		if err = tx.Commit(r.Context()); err != nil {
			return nil, err
		}
		e.ref = ref
	}
	e.attempt++
	tx, err := e.guard(r.Context())
	if err != nil {
		return nil, err
	}
	var once sync.Once
	var commitErr error
	release := func() {
		once.Do(func() {
			bounded, end := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			defer end()
			commitErr = tx.Commit(bounded)
		})
	}
	defer release()
	if _, err = tx.Exec(r.Context(), `UPDATE learning_cli_model_requests SET state='running',dispatched=TRUE WHERE privacy_generation=$1 AND actor_device_id=$2 AND request_id=$3`, e.meta.Generation, e.actor.Device.ID, e.meta.RequestID); err != nil {
		return nil, err
	}
	// Dispatch is committed before bytes can reach the upstream. The short
	// device/request gate stays held only through the actual request write.
	if err = e.service.Usage.Dispatch(r.Context(), e.ref); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(r.Context())
	timer := time.AfterFunc(5*time.Second, cancel)
	trace := &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { timer.Stop(); release() }}
	r = r.WithContext(httptrace.WithClientTrace(ctx, trace))
	response, err := e.service.Usage.Observe(ctx, e.ref, t.next, r)
	timer.Stop()
	release()
	if err != nil {
		cancel()
		return response, err
	}
	if commitErr != nil {
		cancel()
		_ = response.Body.Close()
		return nil, gatewayError("gateway_unavailable")
	}
	response.Body = &cancelBody{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error { err := b.ReadCloser.Close(); b.cancel(); return err }
