// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the
// License is located at
//
// http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// Package session starts the session.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aws/SSMCLI/src/datachannel"
	"github.com/aws/SSMCLI/src/log"
	"github.com/aws/SSMCLI/src/message"
	"github.com/aws/SSMCLI/src/retry"
	"github.com/aws/SSMCLI/src/sdkutil"
	"github.com/aws/SSMCLI/src/sessionmanagerplugin/session/sessionutil"
	"github.com/aws/SSMCLI/src/version"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/twinj/uuid"
)

const (
	LegacyArgumentLength  = 4
	StartSessionOperation = "StartSession"
	ClientTimeoutSecond   = time.Duration(10 * time.Second)
)

var SessionRegistry = map[string]ISessionPlugin{}

type ISessionPlugin interface {
	SetSessionHandlers(log.T) error
	ProcessStreamMessagePayload(log log.T, streamDataMessage message.ClientMessage) (isHandlerReady bool, err error)
	Initialize(log log.T, sessionVar *Session)
	Stop()
	Name() string
}

type ISession interface {
	Execute(log.T) error
	OpenDataChannel(log.T) error
	ProcessFirstMessage(log log.T, outputMessage message.ClientMessage) (isHandlerReady bool, err error)
	Stop()
	GetResumeSessionParams(log.T) (string, error)
	ResumeSessionHandler(log.T) error
	TerminateSession(log.T) error
}

func init() {
	SessionRegistry = make(map[string]ISessionPlugin)
}

func Register(session ISessionPlugin) {
	SessionRegistry[session.Name()] = session
}

type Session struct {
	DataChannel           datachannel.IDataChannel
	SessionId             string
	StreamUrl             string
	TokenValue            string
	IsAwsCliUpgradeNeeded bool
	Endpoint              string
	ClientId              string
	TargetId              string
	sdk                   *ssm.SSM
	retryParams           retry.RepeatableExponentialRetryer
	SessionType           string
	SessionProperties     interface{}
	DisplayMode           sessionutil.DisplayMode
	Timeout               time.Duration
}

//startSession create the datachannel for session
var startSession = func(session *Session, log log.T) error {
	return session.Execute(log)
}

//setSessionHandlersWithSessionType set session handlers based on session subtype
var setSessionHandlersWithSessionType = func(session *Session, log log.T) error {
	// SessionType is set inside DataChannel
	sessionSubType := SessionRegistry[session.SessionType]
	sessionSubType.Initialize(log, session)
	return sessionSubType.SetSessionHandlers(log)
}

// ValidateInputAndStartSession validates input sent from AWS CLI and starts a session if validation is successful.
// AWS CLI sends input in the order of
// args[0] will be path of executable (ignored)
// args[1] is session response
// args[2] is client region
// args[3] is operation name
// args[4] is profile name from aws credentials/config files
// args[5] is parameters input to aws cli for StartSession api
// args[6] is endpoint for ssm service
func ValidateInputAndStartSession(args []string, out io.Writer) {
	var (
		err                error
		session            Session
		startSessionOutput ssm.StartSessionOutput
		response           []byte
		region             string
		operationName      string
		profile            string
		ssmEndpoint        string
		target             string
	)
	log := log.Logger(true, "session-manager-plugin")
	uuid.SwitchFormat(uuid.CleanHyphen)

	if len(args) == 1 {
		fmt.Fprint(out, "\nThe Session Manager plugin was installed successfully. "+
			"Use the AWS CLI to start a session.\n\n")
		return
	} else if len(args) == 2 && args[1] == "--version" {
		fmt.Fprintf(out, "%s\n", string(version.Version))
		return
	} else if len(args) >= 2 && len(args) < LegacyArgumentLength {
		fmt.Fprintf(out, "\nUnknown operation %s. \nUse "+
			"session-manager-plugin --version to check the version.\n\n", string(args[1]))
		return

	} else if len(args) == LegacyArgumentLength {
		// If arguments do not have Profile passed from AWS CLI to Session-Manager-Plugin then
		// should be upgraded to use Session Manager encryption feature
		session.IsAwsCliUpgradeNeeded = true
	}

	for argsIndex := 1; argsIndex < len(args); argsIndex++ {
		switch argsIndex {
		case 1:
			response = []byte(args[1])
		case 2:
			region = args[2]
		case 3:
			operationName = args[3]
		case 4:
			profile = args[4]
		case 5:
			// args[5] is parameters input to aws cli for StartSession api call
			startSessionRequest := make(map[string]interface{})
			json.Unmarshal([]byte(args[5]), &startSessionRequest)
			target = startSessionRequest["Target"].(string)
		case 6:
			ssmEndpoint = args[6]
		}
	}
	sdkutil.SetRegionAndProfile(region, profile)
	clientId := uuid.NewV4().String()

	switch operationName {
	case StartSessionOperation:
		if err = json.Unmarshal(response, &startSessionOutput); err != nil {
			log.Errorf("Cannot perform start session: %v", err)
			fmt.Fprintf(out, "Cannot perform start session: %v\n", err)
			return
		}

		session.SessionId = *startSessionOutput.SessionId
		session.StreamUrl = *startSessionOutput.StreamUrl
		session.TokenValue = *startSessionOutput.TokenValue
		session.Endpoint = ssmEndpoint
		session.ClientId = clientId
		session.TargetId = target
		session.DataChannel = &datachannel.DataChannel{}
		session.Timeout = ClientTimeoutSecond

	default:
		fmt.Fprint(out, "Invalid Operation")
		return
	}

	if err = startSession(&session, log); err != nil {
		log.Errorf("Cannot perform start session: %v", err)
		fmt.Fprintf(out, "Cannot perform start session: %v\n", err)
		return
	}
}

//Execute create data channel and start the session
func (s *Session) Execute(log log.T) (err error) {
	fmt.Fprintf(os.Stdout, "\nStarting session with SessionId: %s\n", s.SessionId)

	// sets the display mode
	s.DisplayMode = sessionutil.NewDisplayMode(log)

	if err = s.OpenDataChannel(log); err != nil {
		log.Errorf("Error in Opening data channel: %v", err)
		return
	}

	select {
	// The session type is set either by handshake or the first packet received.
	case isSessionTypeSet := <-s.DataChannel.IsSessionTypeSet():
		if !isSessionTypeSet {
			log.Errorf("unable to  SessionType for session %s", s.SessionId)
			return errors.New("unable to determine SessionType")
		} else {
			s.SessionType = s.DataChannel.GetSessionType()
			s.SessionProperties = s.DataChannel.GetSessionProperties()
			if err = setSessionHandlersWithSessionType(s, log); err != nil {
				log.Errorf("Session ending with error: %v", err)
				return
			}
		}
	case <-time.After(s.Timeout):
		log.Errorf("client timeout: unable to receive message %s", s.SessionId)
		s.TerminateSession(log)
		return errors.New("client timeout: unable to receive message " + s.SessionId)
	}
	return
}