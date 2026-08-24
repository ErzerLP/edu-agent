package fixture

import "time"

type HandlerOptions struct {
	APIKey          string
	ControlKey      string
	Controller      *Controller
	MaxRequestBytes int64
	TimeoutDelay    time.Duration
}

func NewHandler(options HandlerOptions) *Handler {
	if options.APIKey == "" {
		options.APIKey = "test-key"
	}
	if options.ControlKey == "" {
		options.ControlKey = "test-control-key"
	}
	if options.Controller == nil {
		options.Controller = NewController()
	}
	if options.MaxRequestBytes <= 0 {
		options.MaxRequestBytes = 1 << 20
	}
	if options.TimeoutDelay <= 0 {
		options.TimeoutDelay = 60 * time.Second
	}
	return &Handler{
		apiKey: options.APIKey, controlKey: options.ControlKey, controller: options.Controller,
		maxRequestBytes: options.MaxRequestBytes, timeoutDelay: options.TimeoutDelay,
	}
}
