package store

import (
	"slices"
	"testing"
)

func TestReadState(t *testing.T) {
	var r ReadState
	r.Mark(5, 3)
	if r.Read(4) || !r.Read(3) || !r.Read(5) {
		t.Fatalf("read %+v", r)
	}
	// 2 and 4 are other members' messages (not in seqs); 3 and 5 are read; 7 isn't.
	r.Advance([]uint64{1, 3, 5, 6, 7}, func(q uint64) bool { return q == 1 || q == 6 })
	if r.Upto != 6 || len(r.Extra) != 0 {
		t.Fatalf("advance %+v", r)
	}
	r.Mark(9)
	r.Advance([]uint64{7, 9}, func(uint64) bool { return false })
	if r.Upto != 6 || !slices.Equal(r.Extra, []uint64{9}) {
		t.Fatalf("stuck at unread 7: %+v", r)
	}
	for q := range uint64(MaxExtra + 10) {
		r.Mark(100 + q)
	}
	if len(r.Extra) != MaxExtra || !r.Read(100+MaxExtra+9) {
		t.Fatalf("extra bound: %d", len(r.Extra))
	}
}

func TestDeliverySeq(t *testing.T) {
	if got := DeliverySeq("$JS.ACK.INBOX_acme.hermes.1.42.7.1700000000.0"); got != 42 {
		t.Fatal(got)
	}
	if got := DeliverySeq("$JS.ACK.dom.hash.INBOX_acme.hermes.1.43.7.1700000000.0.tok"); got != 43 {
		t.Fatal(got)
	}
	if DeliverySeq("junk") != 0 {
		t.Fatal("junk")
	}
}
