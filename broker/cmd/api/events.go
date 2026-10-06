package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"logwolf-toolbox/data"
	"logwolf-toolbox/event"
	"net/http"
	"time"
)

// publisher is where the broker sends events: an *event.Emitter, or a fake in
// tests.
type publisher interface {
	Publish(ctx context.Context, msgs ...event.Message) error
	Check() error
}

// publishTimeout bounds how long a request waits for RabbitMQ to confirm its
// events.
const publishTimeout = 10 * time.Second

// eventMessage encodes one event the way the listener reads it, under its
// severity's routing key.
func eventMessage(p data.JSONLogPayload) (event.Message, error) {
	body, err := json.Marshal(event.Payload{Action: "log", Log: p})
	if err != nil {
		return event.Message{}, err
	}
	return event.Message{Body: body, RoutingKey: data.SeverityRoutingKey(p.Severity)}, nil
}

// publishEvents queues events and answers the request: 202 once RabbitMQ has
// confirmed every one of them, 503 otherwise. A 202 used to mean only that the
// broker had tried.
//
// Every severity is normalized first (see data.NormalizeSeverity): "ERROR" is
// stored as "error", and an event with any other severity is a 400. All of
// them are checked and encoded before any is sent, so a bad one sends none. A
// batch that fails part-way may have queued some; a client that retries it can
// store those twice.
//
// Then the events are held to the monthly event quota of the project's
// organization (limitQuota), the dashboard's included, since they count toward
// it as well. Those sent with an API key are also held to the key's rate
// (limitIngest), a token per event; those the dashboard sends have no key and
// no rate. A request refused by either queues nothing.
//
// Once RabbitMQ has confirmed them, the events are counted against their
// project's usage, with the size of their queued messages as their bytes (see
// usageMeter): every event the broker answers 202 for, the dashboard's
// included, and none it does not.
func (app *Config) publishEvents(w http.ResponseWriter, r *http.Request, payloads ...data.JSONLogPayload) {
	msgs := make([]event.Message, len(payloads))
	for i, p := range payloads {
		severity, err := data.NormalizeSeverity(p.Severity)
		if err != nil {
			if len(payloads) > 1 {
				err = fmt.Errorf("event %d: %w", i, err)
			}
			app.errorJSON(w, err, http.StatusBadRequest)
			return
		}
		p.Severity = severity

		m, err := eventMessage(p)
		if err != nil {
			app.errorJSON(w, err, http.StatusBadRequest)
			return
		}
		msgs[i] = m
	}

	// The handlers file every event under the one project of the request.
	projectID := payloads[0].ProjectID

	// The quota comes first, so a request it refuses spends none of the
	// key's rate; one the rate refuses gives back what it reserved.
	reservation, ok := app.limitQuota(w, r, projectID, len(msgs))
	if !ok {
		return
	}
	if keyIDFromContext(r) != "" && !app.limitIngest(w, r, len(msgs)) {
		releaseQuota(reservation)
		return
	}

	if app.Events == nil {
		releaseQuota(reservation)
		app.errorJSON(w, fmt.Errorf("event queue unavailable"), http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), publishTimeout)
	defer cancel()
	if err := app.Events.Publish(ctx, msgs...); err != nil {
		// Some of a batch may have been queued; giving them all back lets
		// the quota trail, never lead, what was ingested.
		releaseQuota(reservation)
		log.Printf(`{"event":"publish","outcome":"error","count":%d,"error":%q}`, len(msgs), err.Error())
		app.errorJSON(w, fmt.Errorf("could not queue the events, try again"), http.StatusServiceUnavailable)
		return
	}

	var size int64
	for _, m := range msgs {
		size += int64(len(m.Body))
	}
	app.Usage.record(projectID, int64(len(msgs)), size)

	app.writeJSON(w, http.StatusAccepted, jsonResponse{Error: false, Message: "OK!"})
}
