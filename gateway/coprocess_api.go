// +build coprocess
// +build !grpc

package gateway

/*
#include <stdio.h>

#include "../coprocess/api.h"

#ifdef ENABLE_PYTHON
#include "../coprocess/python/dispatcher.h"
#include "../coprocess/python/binding.h"
#endif
*/
import "C"

import (
	"github.com/Sirupsen/logrus"

	"github.com/ins-tykgw/tyk/apidef"
	"github.com/ins-tykgw/tyk/storage"
)

// CoProcessDefaultKeyPrefix is used as a key prefix for this CP.
const CoProcessDefaultKeyPrefix = "coprocess-data:"

// TODO: implement INCR, DECR?

// TykStoreData is a CoProcess API function for storing data.
//export TykStoreData
func TykStoreData(CKey, CValue *C.char, CTTL C.int) {
	key := C.GoString(CKey)
	value := C.GoString(CValue)
	ttl := int64(CTTL)

	store := storage.RedisCluster{KeyPrefix: CoProcessDefaultKeyPrefix}
	store.SetKey(key, value, ttl)
}

// TykGetData is a CoProcess API function for fetching data.
//export TykGetData
func TykGetData(CKey *C.char) *C.char {
	key := C.GoString(CKey)

	store := storage.RedisCluster{KeyPrefix: CoProcessDefaultKeyPrefix}
	// TODO: return error
	val, _ := store.GetKey(key)
	return C.CString(val)
}

// TykTriggerEvent is a CoProcess API function for triggering Tyk system events.
//export TykTriggerEvent
func TykTriggerEvent(CEventName, CPayload *C.char) {
	eventName := C.GoString(CEventName)
	payload := C.GoString(CPayload)

	FireSystemEvent(apidef.TykEvent(eventName), EventMetaDefault{
		Message: payload,
	})
}

// CoProcessLog is a bridge for using Tyk log from CP.
//export CoProcessLog
func CoProcessLog(CMessage, CLogLevel *C.char) {
	message := C.GoString(CMessage)
	logLevel := C.GoString(CLogLevel)
	switch logLevel {
	case "debug":
		log.WithFields(logrus.Fields{
			"prefix": CoProcessName,
		}).Debug(message)
	case "error":
		log.WithFields(logrus.Fields{
			"prefix": CoProcessName,
		}).Error(message)
	case "warning":
		log.WithFields(logrus.Fields{
			"prefix": CoProcessName,
		}).Warning(message)
	default:
		log.WithFields(logrus.Fields{
			"prefix": CoProcessName,
		}).Info(message)
	}
}
