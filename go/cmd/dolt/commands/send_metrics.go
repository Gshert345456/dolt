// Copyright 2019 Liquidata, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package commands

import (
	"context"
	"time"

	"github.com/fatih/color"

	"github.com/liquidata-inc/dolt/go/cmd/dolt/cli"
	"github.com/liquidata-inc/dolt/go/libraries/doltcore/dbfactory"
	"github.com/liquidata-inc/dolt/go/libraries/doltcore/env"
	"github.com/liquidata-inc/dolt/go/libraries/events"
	"github.com/liquidata-inc/dolt/go/libraries/utils/argparser"
)

// SendMetricsCommand is the command used for sending metrics
const (
	SendMetricsCommand  = "send-metrics"
	outputFlag          = "output"
	sendMetricsShortDec = "Send metrics to the events server or print them to stdout"
)

// SendMetrics is the commandFunc used that flushes the events to the grpc service
func SendMetrics(ctx context.Context, commandStr string, args []string, dEnv *env.DoltEnv) int {
	ap := argparser.NewArgParser()
	ap.SupportsFlag(outputFlag, "o", "Flush events to stdout.")

	help, _ := cli.HelpAndUsagePrinters(commandStr, sendMetricsShortDec, "", []string{}, ap)
	apr := cli.ParseArgs(ap, args, help)

	disabled, err := events.AreMetricsDisabled(dEnv)
	if err != nil {
		// log.Print(err)
		return 1
	}

	if disabled {
		cli.Println(color.CyanString("Sending metrics is currently disabled\n"))
		return 0
	}

	if !disabled {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()

		root, err := dEnv.GetUserHomeDir()
		if err != nil {
			// log.Print(err)
			return 1
		}

		dolt := dbfactory.DoltDir

		var flusher events.Flusher

		if apr.Contains(outputFlag) {
			flusher = events.NewIOFlusher(dEnv.FS, root, dolt, dEnv)
		} else {
			flusher = events.NewGrpcEventFlusher(dEnv.FS, root, dolt, dEnv)
		}

		err = flusher.Flush(ctx)

		if err != nil {
			if err == events.ErrFileLocked {
				return 2
			}

			return 1
		}

		return 0
	}

	return 1
}
