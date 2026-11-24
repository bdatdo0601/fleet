package waiter

import (
	"encoding/json"
)

func unmarshalJSON(data []byte, dst any) error {
	return json.Unmarshal(data, dst)
}
