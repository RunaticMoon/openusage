package telemetry

import "encoding/json"

func isAuthoritativeMessage(system SourceSystem, channel SourceChannel, schema string, eventType EventType, messageID string) bool {
	return isOpenCodeV2Message(system, channel, schema, eventType, messageID) ||
		(system == "dsh" && channel == SourceChannelJSONL && schema == "dsh_jsonl_v4" && eventType == EventTypeMessageUsage && messageID != "")
}

func authoritativeRevision(system SourceSystem, payload []byte) int64 {
	if system != "dsh" {
		return openCodeRevision(payload)
	}
	var record struct {
		DSH struct {
			Revision json.Number `json:"revision"`
		} `json:"dsh"`
	}
	if json.Unmarshal(payload, &record) != nil {
		return 0
	}
	revision, _ := record.DSH.Revision.Int64()
	return revision
}
