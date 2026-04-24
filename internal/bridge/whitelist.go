package bridge

type Whitelist struct {
	ids     map[int64]struct{}
	chatID  int64
}

func NewWhitelist(userIDs []int64, chatID int64) *Whitelist {
	w := &Whitelist{ids: make(map[int64]struct{}, len(userIDs)), chatID: chatID}
	for _, id := range userIDs {
		w.ids[id] = struct{}{}
	}
	return w
}

type GateResult int

const (
	GateAllowed GateResult = iota
	GateRejectUser
	GateRejectChat
)

func (w *Whitelist) Check(userID, chatID int64) GateResult {
	if _, ok := w.ids[userID]; !ok {
		return GateRejectUser
	}
	if chatID != w.chatID {
		return GateRejectChat
	}
	return GateAllowed
}
