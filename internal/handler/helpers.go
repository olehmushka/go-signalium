package handler

import (
	"time"

	googleuuid "github.com/google/uuid"
	"github.com/palantir/pkg/datetime"
	palantiruuid "github.com/palantir/pkg/uuid"

	"github.com/olehmushka/go-signalium/internal/domain"
	signalapi "github.com/olehmushka/go-signalium/internal/generated/signalium/api"
)

// googleUUID converts the conjure-runtime UUID type to the google/uuid type
// used by the repo. Both are [16]byte under the hood.
func googleUUID(id palantiruuid.UUID) googleuuid.UUID {
	return googleuuid.UUID(id)
}

// conjureToDomainStatus drops the conjure struct wrapper into a plain domain
// status pointer. nil in → nil out so the filter stays optional.
func conjureToDomainStatus(s *signalapi.SignalMessageStatus) *domain.SignalMessageStatus {
	if s == nil {
		return nil
	}
	v := domain.SignalMessageStatus(s.Value())
	return &v
}

// dateTimeToTime unwraps a conjure datetime pointer into a time pointer. nil
// stays nil; zero values are passed through so the SQL narg becomes NULL.
func dateTimeToTime(d *datetime.DateTime) *time.Time {
	if d == nil {
		return nil
	}
	t := time.Time(*d)
	return &t
}

// intPtrToInt32 narrows an int pointer to an int32 pointer. The conjure
// generator emits int for query params; the repo signature uses int32 to
// match sqlc's narg<int>.
func intPtrToInt32(v *int) *int32 {
	if v == nil {
		return nil
	}
	n := int32(*v)
	return &n
}

// coalescePositive returns *v when v is non-nil and > 0; otherwise fallback.
// Used to apply the default page size when ?limit is missing or non-positive.
func coalescePositive(v *int, fallback int32) int32 {
	if v == nil || *v <= 0 {
		return fallback
	}
	return int32(*v)
}

// coalesceNonNegative returns int32(*v) when v is non-nil and >= 0; otherwise 0.
// Used for ?offset where a negative value is meaningless.
func coalesceNonNegative(v *int) int32 {
	if v == nil || *v < 0 {
		return 0
	}
	return int32(*v)
}
