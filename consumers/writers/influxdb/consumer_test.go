// Copyright (c) Mainflux
// SPDX-License-Identifier: Apache-2.0

package influxdb_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/gofrs/uuid"
	influxdata "github.com/influxdata/influxdb-client-go/v2"
	writer "github.com/mainflux/mainflux/consumers/writers/influxdb"
	log "github.com/mainflux/mainflux/logger"
	"github.com/mainflux/mainflux/pkg/errors"
	"github.com/mainflux/mainflux/pkg/transformers/json"
	"github.com/mainflux/mainflux/pkg/transformers/senml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const valueFields = 5

var (
	hostPort          string
	testLog, _        = log.New(os.Stdout, log.Info.String())
	testOrg           = "test"
	testBucket        = "test"
	testMainfluxToken = "test"
	testMainfluxUrl   = "test"
	streamsSize       = 250
	selectMsgs        = "from(bucket: \"test\") |> range(start: -1d) |> filter(fn: (r) => r._field == \"value\")"
	client            influxdata.Client
	subtopic          = "topic"
)

var (
	v       float64 = 5
	stringV         = "value"
	boolV           = true
	dataV           = "base64"
	sum     float64 = 42
)

// This is utility function to query the database.
func queryDB(cmd string) (res []map[string]interface{}, err error) {
	var ctx = context.Background()
	response, err := client.QueryAPI(testOrg).Query(ctx, cmd)
	if err != nil {
		return nil, err
	}
	defer response.Close()

	if response.Err() != nil {
		return nil, response.Err()
	}
	// There is only one query, so only one result and
	// all data are stored in the same series.
	for response.Next() {
		res = append(res, response.Record().Values())
	}
	return
}

func cleanDB() error {
	var ctx = context.Background()
	err := client.DeleteAPI().DeleteWithName(ctx, testOrg, testBucket, time.Unix(0, 0), time.Now(), "")
	return err
}

func TestSaveSenml(t *testing.T) {
	logger, _ := log.New(os.Stdout, log.Info.String())
	repo := writer.New(client, testOrg, testBucket, testMainfluxToken, testMainfluxUrl, logger)

	cases := []struct {
		desc         string
		msgsNum      int
		expectedSize int
	}{
		{
			desc:         "save a single message",
			msgsNum:      1,
			expectedSize: 1,
		},
		{
			desc:         "save a batch of messages",
			msgsNum:      streamsSize,
			expectedSize: streamsSize,
		},
	}

	for _, tc := range cases {
		// Clean previously saved messages.
		err := cleanDB()
		require.Nil(t, err, fmt.Sprintf("Cleaning data from InfluxDB expected to succeed: %s.\n", err))
		// Create a batch of messages.
		now := time.Now().UnixNano()
		msg := senml.Message{
			Channel:    "45",
			Publisher:  "2580",
			Protocol:   "http",
			Name:       "test name",
			Unit:       "km",
			UpdateTime: 5456565466,
		}
		var msgs []senml.Message

		for i := 0; i < tc.msgsNum; i++ {
			// Mix possible values as well as value sum.
			count := i % valueFields
			switch count {
			case 0:
				msg.Subtopic = subtopic
				msg.Value = &v
			case 1:
				msg.BoolValue = &boolV
			case 2:
				msg.StringValue = &stringV
			case 3:
				msg.DataValue = &dataV
			case 4:
				msg.Sum = &sum
			}

			msg.Time = float64(now)/float64(1e9) - float64(i)
			msgs = append(msgs, msg)
		}

		err = repo.Consume(msgs)
		assert.Nil(t, err, fmt.Sprintf("Save operation expected to succeed: %s.\n", err))

		row, err := queryDB(selectMsgs)
		assert.Nil(t, err, fmt.Sprintf("Querying InfluxDB to retrieve data expected to succeed: %s.\n", err))

		count := len(row)
		assert.Equal(t, tc.expectedSize, count, fmt.Sprintf("Expected to have %d messages saved, found %d instead.\n", tc.expectedSize, count))
	}
}

func TestSaveJSON(t *testing.T) {
	logger, _ := log.New(os.Stdout, log.Info.String())
	repo := writer.New(client, testOrg, testBucket, testMainfluxToken, testMainfluxUrl, logger)

	chid, err := uuid.NewV4()
	require.Nil(t, err, fmt.Sprintf("got unexpected error: %s", err))
	pubid, err := uuid.NewV4()
	require.Nil(t, err, fmt.Sprintf("got unexpected error: %s", err))

	msg := json.Message{
		Channel:   chid.String(),
		Publisher: pubid.String(),
		Created:   time.Now().UnixNano(),
		Subtopic:  "subtopic/format/json",
		Protocol:  "mqtt",
		Payload: map[string]interface{}{
			"field_1": 123,
			"field_2": "value",
			"field_3": false,
			"value":   12.344,
			"field_5": map[string]interface{}{
				// "field_1": "value",
				"field_2": 42,
			},
			"deviceName":  "device-123",
			"measurement": "lighting",
		},
	}

	invalidKeySepMsg := msg
	invalidKeySepMsg.Payload = map[string]interface{}{
		"field_1": 123,
		"field_2": "value",
		"field_3": false,
		"field_4": 12.344,
		"field_5": map[string]interface{}{
			"field_1": "value",
			"field_2": 42,
		},
		"field_6/field_7": "value",
	}
	invalidKeyNameMsg := msg
	invalidKeyNameMsg.Payload = map[string]interface{}{
		"field_1": 123,
		"field_2": "value",
		"field_3": false,
		"field_4": 12.344,
		"field_5": map[string]interface{}{
			"field_1": "value",
			"field_2": 42,
		},
		"publisher": "value",
	}

	now := time.Now().UnixNano()
	msgs := json.Messages{
		Format: "json",
	}
	invalidKeySepMsgs := json.Messages{
		Format: "json",
	}
	invalidKeyNameMsgs := json.Messages{
		Format: "json",
	}

	for i := 0; i < streamsSize; i++ {
		msg.Created = now + int64(i)
		msgs.Data = append(msgs.Data, msg)
		invalidKeySepMsgs.Data = append(invalidKeySepMsgs.Data, invalidKeySepMsg)
		invalidKeyNameMsgs.Data = append(invalidKeyNameMsgs.Data, invalidKeyNameMsg)
	}

	cases := []struct {
		desc string
		msgs json.Messages
		err  error
	}{
		{
			desc: "consume valid json messages",
			msgs: msgs,
			err:  nil,
		},
		{
			desc: "consume invalid json messages containing invalid key separator",
			msgs: invalidKeySepMsgs,
			err:  json.ErrInvalidKey,
		},
		{
			desc: "consume invalid json messages containing invalid key name",
			msgs: invalidKeySepMsgs,
			err:  json.ErrInvalidKey,
		},
	}
	for _, tc := range cases {
		err = repo.Consume(tc.msgs)
		assert.True(t, errors.Contains(err, tc.err), fmt.Sprintf("%s expected %s, got %s", tc.desc, tc.err, err))

		row, err := queryDB(selectMsgs)
		assert.Nil(t, err, fmt.Sprintf("Querying InfluxDB to retrieve data expected to succeed: %s.\n", err))

		count := len(row)
		assert.Equal(t, streamsSize, count, fmt.Sprintf("Expected to have %d messages saved, found %d instead.\n", streamsSize, count))
	}
}
