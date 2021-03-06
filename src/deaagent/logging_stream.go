package deaagent

import (
	"code.google.com/p/gogoprotobuf/proto"
	"github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/loggregatorlib/cfcomponent/instrumentation"
	"github.com/cloudfoundry/loggregatorlib/emitter"
	"github.com/cloudfoundry/loggregatorlib/logmessage"
	"net"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"
)

type loggingStream struct {
	task             task
	emitter          emitter.Emitter
	logger           *gosteno.Logger
	messageType      logmessage.LogMessage_MessageType
	messagesReceived *uint64
	bytesReceived    *uint64
}

const bufferSize = 4096

func newLoggingStream(task task, emitter emitter.Emitter, logger *gosteno.Logger, messageType logmessage.LogMessage_MessageType) (ls *loggingStream) {
	return &loggingStream{task, emitter, logger, messageType, new(uint64), new(uint64)}
}

func (ls loggingStream) listen() {
	newLogMessage := func(message []byte) *logmessage.LogMessage {
		currentTime := time.Now()
		sourceName := ls.task.sourceName
		sourceId := strconv.FormatUint(ls.task.index, 10)

		return &logmessage.LogMessage{
			Message:     message,
			AppId:       proto.String(ls.task.applicationId),
			DrainUrls:   ls.task.drainUrls,
			MessageType: &ls.messageType,
			SourceName:  &sourceName,
			SourceId:    &sourceId,
			Timestamp:   proto.Int64(currentTime.UnixNano()),
		}
	}

	socket := func(messageType logmessage.LogMessage_MessageType) (net.Conn, error) {
		return net.Dial("unix", filepath.Join(ls.task.identifier(), socketName(messageType)))
	}

	go func() {
		var connection net.Conn
		i := 0
		for {
			var err error
			connection, err = socket(ls.messageType)
			if err == nil {
				break
			} else {
				ls.logger.Errorf("Error while dialing into socket %s, %s, %s", ls.messageType, ls.task.identifier(), err)
				i += 1
				if i < 86400 {
					time.Sleep(1 * time.Second)
				} else {
					ls.logger.Errorf("Giving up after %d tries dialing into socket %s, %s, %s", i, ls.messageType, ls.task.identifier(), err)
					return
				}
			}
		}

		defer func() {
			connection.Close()
			ls.logger.Infof("Stopped reading from socket %s, %s", ls.messageType, ls.task.identifier())
		}()

		buffer := make([]byte, bufferSize)
		totalRead := 0
		const _skippedBytes = 4

		for {
			readCount, err := connection.Read(buffer)
			if err != nil {
				ls.logger.Infof("Error while reading from socket %s, %s, %s", ls.messageType, ls.task.identifier(), err)
				break
			}

			ls.logger.Debugf("Read %d bytes from task socket %s, %s", readCount, ls.messageType, ls.task.identifier())
			atomic.AddUint64(ls.messagesReceived, 1)
			atomic.AddUint64(ls.bytesReceived, uint64(readCount))

			skipCount := 0
			if totalRead < _skippedBytes {
				skipCount = _skippedBytes - totalRead
				if skipCount > readCount {
					skipCount = readCount
				}

				ls.logger.Debugf("Skipping %d bytes from task socket (offset message)", skipCount)
			}
			totalRead += readCount

			if readCount > skipCount {
				rawMessageBytes := make([]byte, readCount-skipCount)
				copy(rawMessageBytes, buffer[skipCount:readCount])

				ls.logger.Debugf("This is the message we just read % 02x", rawMessageBytes)
				logMessage := newLogMessage(rawMessageBytes)

				ls.emitter.EmitLogMessage(logMessage)

				ls.logger.Debugf("Sent %d bytes to loggregator client from %s, %s", readCount-skipCount, ls.messageType, ls.task.identifier())
			}

			runtime.Gosched()
		}
	}()
}

func (ls loggingStream) Emit() instrumentation.Context {
	return instrumentation.Context{Name: "loggingStream:" + ls.task.wardenContainerPath + " type " + socketName(ls.messageType),
		Metrics: []instrumentation.Metric{
			instrumentation.Metric{Name: "receivedMessageCount", Value: atomic.LoadUint64(ls.messagesReceived)},
			instrumentation.Metric{Name: "receivedByteCount", Value: atomic.LoadUint64(ls.bytesReceived)},
		},
	}
}

func socketName(messageType logmessage.LogMessage_MessageType) string {
	if messageType == logmessage.LogMessage_OUT {
		return "stdout.sock"
	}
	return "stderr.sock"
}
