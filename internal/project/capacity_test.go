package project

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/kopia/kopia/repo/blob"
	"github.com/redthing1/lode/internal/model"
)

func TestDefaultCapacityPolicyAndCaptureBudget(t *testing.T) {
	policy := DefaultCapacityPolicy()
	if policy.ReserveBytes != 1<<30 {
		t.Fatalf("default reserve = %d, want 1 GiB", policy.ReserveBytes)
	}
	if err := policy.Validate(); err != nil {
		t.Fatalf("default policy invalid: %v", err)
	}
	if err := (CapacityPolicy{}).Validate(); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("zero reserve validation error = %v", err)
	}
	if err := (CapacityPolicy{ReserveBytes: math.MaxInt64}).Validate(); err != nil {
		t.Fatalf("max SQLite reserve was rejected: %v", err)
	}
	if err := (CapacityPolicy{ReserveBytes: math.MaxUint64}).Validate(); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("reserve above SQLite range validation error = %v", err)
	}

	changes := []Change{
		{Path: "a", Kind: model.File, Size: 13},
		{Path: "b", Kind: model.Directory},
		{Path: "c", Kind: model.Absent},
		{Path: "d", Kind: model.File, Size: 21},
	}
	got, err := captureBudget(changes)
	if err != nil || got != 68 {
		t.Fatalf("captureBudget() = %d, %v; want 68", got, err)
	}
	if _, err := captureBudget([]Change{{Path: "bad", Kind: model.File, Size: -1}}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("negative size error = %v, want ErrInvalidRequest", err)
	}
	if _, err := captureBudget([]Change{{Path: "huge-a", Kind: model.File, Size: math.MaxInt64}, {Path: "huge-b", Kind: model.File, Size: 1}}); !errors.Is(err, ErrCapacityEstimateOverflow) {
		t.Fatalf("overflowing source estimate error = %v", err)
	}
}

func TestCapacityPolicyPersistsAndRejectsZeroReserve(t *testing.T) {
	local := openTestLocal(t)
	got, err := local.Service.CapacityPolicy(context.Background())
	if err != nil || got != DefaultCapacityPolicy() {
		t.Fatalf("initial CapacityPolicy() = %#v, %v; want default", got, err)
	}
	want := CapacityPolicy{ReserveBytes: 2 << 30}
	if err := local.Service.SetCapacityPolicy(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, err = local.Service.CapacityPolicy(context.Background())
	if err != nil || got != want {
		t.Fatalf("updated CapacityPolicy() = %#v, %v; want %#v", got, err, want)
	}
	if err := local.Service.SetCapacityPolicy(context.Background(), CapacityPolicy{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("SetCapacityPolicy zero reserve error = %v", err)
	}
	if err := local.Service.SetCapacityPolicy(context.Background(), CapacityPolicy{ReserveBytes: math.MaxUint64}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("SetCapacityPolicy oversized reserve error = %v", err)
	}
	got, err = local.Service.CapacityPolicy(context.Background())
	if err != nil || got != want {
		t.Fatalf("invalid update changed policy: %#v, %v", got, err)
	}
}

func TestAdmitCapacityChecksReserveAndReportedFreeBytes(t *testing.T) {
	policy := CapacityPolicy{ReserveBytes: 100}
	var calls int
	probe := func(context.Context) (blob.Capacity, error) {
		calls++
		return blob.Capacity{SizeB: 1000, FreeB: 150}, nil
	}
	if err := admitCapacity(context.Background(), probe, policy, 50); err != nil {
		t.Fatalf("admit exact available capacity: %v", err)
	}
	if err := admitCapacity(context.Background(), probe, policy, 51); !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("one-byte short admission error = %v", err)
	} else {
		var capacityErr *CapacityError
		if !errors.As(err, &capacityErr) || capacityErr.AvailableBytes != 150 || capacityErr.RequiredBytes != 151 {
			t.Fatalf("capacity error details = %#v", err)
		}
	}
	if calls != 2 {
		t.Fatalf("capacity probe called %d times, want 2", calls)
	}
}

func TestAdmitCapacityFailsClosedWhenUnknown(t *testing.T) {
	providerErr := errors.New("statfs unavailable")
	probe := func(context.Context) (blob.Capacity, error) { return blob.Capacity{}, providerErr }
	if err := admitCapacity(context.Background(), probe, DefaultCapacityPolicy(), 0); !errors.Is(err, ErrCapacityUnknown) || !errors.Is(err, providerErr) {
		t.Fatalf("unknown capacity error = %v", err)
	}
	if err := admitCapacity(context.Background(), nil, DefaultCapacityPolicy(), 0); !errors.Is(err, ErrCapacityUnknown) {
		t.Fatalf("nil provider error = %v", err)
	}
	if err := admitCapacity(context.Background(), func(context.Context) (blob.Capacity, error) {
		return blob.Capacity{}, nil
	}, DefaultCapacityPolicy(), 0); !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("zero reported free capacity error = %v", err)
	}
}

func TestAdmitCapacityRejectsArithmeticOverflow(t *testing.T) {
	probeCalled := false
	probe := func(context.Context) (blob.Capacity, error) {
		probeCalled = true
		return blob.Capacity{FreeB: math.MaxUint64}, nil
	}
	if err := admitCapacity(context.Background(), probe, CapacityPolicy{ReserveBytes: 1}, math.MaxUint64); !errors.Is(err, ErrCapacityEstimateOverflow) {
		t.Fatalf("overflowing required-byte estimate error = %v", err)
	}
	if !probeCalled {
		t.Fatal("capacity probe was not called")
	}
}
