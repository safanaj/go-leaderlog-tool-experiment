package main

import "time"

// from byron-genesis.json
// $ cat byron-genesis.json |jq .startTime
// 1506203091
// $ date --utc --date="@$(cat byron-genesis.json |jq .startTime)"
// Sat Sep 23 21:44:51 UTC 2017 - 2017-09-23T21:44:51Z
// $ cat /opt/cardano/mainnet/bp/byron-genesis.json |jq .protocolConsts.k
// 2160
// $ cat byron-genesis.json |jq .blockVersionData.slotDuration -r
// 20000

// from shelley-genesis.json
// "epochLength": 432000, // slots per epoch, so (3600 * 24 * 5) seconds per epoch, same as 5 days per epoch
// "slotLength": 1, // one second per slot
// "systemStart": "2017-09-23T21:44:51Z", // is in time.RFC3339 format
// systemStart corresponds to the unix epoch 1506203091

const (
	systemStartUnixEpoch = 1506203091

	byronProtocolConstantK = 2160
	byronEpochLength       = 10 * byronProtocolConstantK
	shelleyTransitionEpoch = 208
	byronSlotDurationSecs  = 20
	byronSlots             = shelleyTransitionEpoch * byronEpochLength
	byronSecs              = byronSlots * byronSlotDurationSecs

	FirstShelleySlotUnixEpoch       = byronSecs + systemStartUnixEpoch // 1596059091 = 1506203091 + 89856000
	FirstShelleySlot          Slot  = byronSlots                       // 4492800 = 208 * 21600
	FirstShelleyEpoch         Epoch = shelleyTransitionEpoch           // 208
	EpochLength                     = 432000

	SlotInEpochForNextNonce Slot = 302400
)

type Epoch uint64
type Slot uint64

func TimeToEpoch(t time.Time) Epoch {
	secondsSinceSystemStart := t.Unix() - systemStartUnixEpoch
	return Epoch(secondsSinceSystemStart / EpochLength)
}
func EpochStartTime(e Epoch) time.Time {
	return time.Unix(systemStartUnixEpoch+(int64(e)*EpochLength), 0)
}
func EpochEndTime(e Epoch) time.Time {
	return time.Unix(systemStartUnixEpoch+((int64(e)+1)*EpochLength)-1, 0)
}

func TimeToSlot(t time.Time) Slot {
	secondsSinceSystemStart := t.Unix() - systemStartUnixEpoch
	return Slot(secondsSinceSystemStart % EpochLength)
}

func TimeToAbsSlot(t time.Time) Slot {
	if t.Unix() < FirstShelleySlotUnixEpoch {
		secondsSinceShelleyStart := t.Unix() - systemStartUnixEpoch
		if secondsSinceShelleyStart < 0 {
			return Slot(0)
		}
		return Slot(secondsSinceShelleyStart / byronSlotDurationSecs)
	}
	secondsSinceShelleyStart := t.Unix() - FirstShelleySlotUnixEpoch
	return Slot(secondsSinceShelleyStart) + FirstShelleySlot
}

func AbsSlotToTime(s Slot) time.Time {
	return time.Unix(int64(FirstShelleySlotUnixEpoch+s-FirstShelleySlot), 0)
}

func AbsSlotToEpoch(s Slot) Epoch {
	if s < FirstShelleySlot {
		return Epoch(s / byronEpochLength)
	}
	return Epoch(int((s-FirstShelleySlot)/EpochLength) + int(FirstShelleyEpoch))
}

func EpochAndSlotToTime(e Epoch, s Slot) time.Time {
	return time.Unix(int64(e)*EpochLength+int64(s), 0)
}

func CurrentEpoch() Epoch      { return TimeToEpoch(time.Now()) }
func CurrentSlot() Slot        { return TimeToAbsSlot(time.Now()) }
func CurrentSlotInEpoch() Slot { return TimeToSlot(time.Now()) }

func getFirstSlotOfEpochSinceShelleyFromAbsSlot(s Slot) Slot {
	if s < FirstShelleySlot {
		return 0
	}
	return s - ((s - FirstShelleySlot) % EpochLength)
}

func getFirstSlotOfEpochSinceShelleyFromEpoch(e Epoch) Slot {
	if e < FirstShelleyEpoch {
		return 0
	}
	return Slot((e-FirstShelleyEpoch)*EpochLength) + FirstShelleySlot
}

func GetFirstSlotOfEpoch(x any) Slot {
	switch v := x.(type) {
	case Epoch:
		return getFirstSlotOfEpochSinceShelleyFromEpoch(v)
	case Slot:
		return getFirstSlotOfEpochSinceShelleyFromAbsSlot(v)
	}
	return Slot(0)
}

func GetEpochAndFirstSlotOfEpochSinceShelley(s Slot) (Epoch, Slot) {
	if s < FirstShelleySlot {
		return AbsSlotToEpoch(s), s - (s % byronEpochLength)
	}
	return AbsSlotToEpoch(s), getFirstSlotOfEpochSinceShelleyFromAbsSlot(s)
}
