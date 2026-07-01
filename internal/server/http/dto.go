package http

import (
	"encoding/json"

	"github.com/vmihailenco/msgpack/v5"
)

// The wire format is JSON (universal for any client); objects are stored
// internally as MessagePack for compactness. Conversion happens only here, at
// the edge.

// jsonToStored converts a JSON object body into the stored MessagePack value.
// A client-supplied "id" is ignored (ids are server-assigned).
func jsonToStored(body []byte) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	delete(m, "id")
	return msgpack.Marshal(m)
}

// storedToObject decodes a stored MessagePack value and injects the object id,
// yielding a JSON-ready map.
func storedToObject(stored []byte, id int64) (map[string]any, error) {
	m := map[string]any{}
	if len(stored) > 0 {
		if err := msgpack.Unmarshal(stored, &m); err != nil {
			return nil, err
		}
	}
	m["id"] = id
	return m, nil
}
