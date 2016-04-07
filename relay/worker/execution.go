package worker

import (
	"encoding/json"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/operable/go-relay/relay"
	"github.com/operable/go-relay/relay/config"
	"github.com/operable/go-relay/relay/engines"
	"github.com/operable/go-relay/relay/messages"
)

func executeCommand(incoming *relay.Incoming) {
	request := &messages.ExecutionRequest{}
	if err := json.Unmarshal(incoming.Payload, request); err != nil {
		log.Errorf("Ignoring malformed execution request: %s.", err)
		return
	}
	bundle := incoming.Relay.GetBundle(request.BundleName())
	response := &messages.ExecutionResponse{}
	if bundle == nil {
		response.Status = "error"
		response.StatusMessage = fmt.Sprintf("Unknown command bundle %s", request.BundleName())
	} else {
		engine, err := engineForBundle(*bundle, *incoming)
		if err != nil {
			response.Status = "error"
			response.StatusMessage = fmt.Sprintf("%s", err)
		} else {
			commandOutput, commandErrors, err := engine.Execute(request, bundle)
			if err != nil {
				response.Status = "error"
				response.StatusMessage = fmt.Sprintf("%s", err)
			} else {
				if len(commandErrors) > 0 {
					response.Status = "error"
					response.StatusMessage = string(commandErrors)
				} else {
					response.Status = "ok"
					response.Body = string(commandOutput)
				}
			}
		}
	}
	responseBytes, _ := json.Marshal(response)
	incoming.Relay.Bus.Publish(request.ReplyTo, responseBytes)
}

func engineForBundle(bundle config.Bundle, incoming relay.Incoming) (engines.Engine, error) {
	if bundle.IsDocker() == true {
		return engines.NewDockerEngine(incoming.Relay.Config.Docker)
	}
	return engines.NewNativeEngine()
}