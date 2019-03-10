package ice

import (
	"errors"
	"io"
	"math/rand"
	"net"
	"sort"
	"testing"

	"github.com/gortc/ice/candidate"
	"github.com/gortc/stun"
)

func newUDPCandidate(t *testing.T, addr HostAddr) candidateAndConn {
	t.Helper()
	zeroPort := net.UDPAddr{
		IP:   addr.IP,
		Port: 0,
	}
	l, err := net.ListenPacket("udp", zeroPort.String())
	if err != nil {
		t.Fatal(err)
	}
	a := l.LocalAddr().(*net.UDPAddr)
	c := Candidate{
		Base: Addr{
			IP:    addr.IP,
			Port:  a.Port,
			Proto: candidate.UDP,
		},
		Type: candidate.Host,
		Addr: Addr{
			IP:    addr.IP,
			Port:  a.Port,
			Proto: candidate.UDP,
		},
		ComponentID: 1,
	}
	c.Foundation = Foundation(&c, Addr{})
	c.Priority = Priority(TypePreference(c.Type), addr.LocalPreference, c.ComponentID)
	return candidateAndConn{
		Candidate: c,
		Conn:      l,
	}
}

type stunMock struct {
	start func(m *stun.Message) error
}

func (s *stunMock) Start(m *stun.Message) error { return s.start(m) }

func mustInit(t *testing.T, a *Agent) {
	t.Helper()
	if err := a.init(); err != nil {
		t.Fatal(err)
	}
}

func TestAgent_processUDP(t *testing.T) {
	t.Run("Blank", func(t *testing.T) {
		a := &Agent{}
		mustInit(t, a)
		t.Run("Not STUN", func(t *testing.T) {
			if err := a.processUDP([]byte{1, 2}, &net.UDPAddr{}); err != errNotSTUNMessage {
				t.Errorf("should be notStun, got %v", err)
			}
		})
		t.Run("No transaction", func(t *testing.T) {
			m := stun.MustBuild(stun.TransactionID, stun.BindingSuccess)
			if err := a.processUDP(m.Raw, &net.UDPAddr{}); err != nil {
				t.Error(err)
			}
		})
		t.Run("Bad STUN", func(t *testing.T) {
			m := stun.MustBuild(stun.TransactionID, stun.BindingSuccess, stun.XORMappedAddress{
				IP: net.IPv4(1, 2, 3, 4),
			}, stun.Fingerprint)
			if err := a.processUDP(m.Raw[:len(m.Raw)-2], &net.UDPAddr{}); err == nil {
				t.Error("should error")
			} else {
				if err == errNotSTUNMessage {
					t.Error("unexpected notStun err")
				}
				t.Log(err)
			}
		})
	})
}

func TestAgent_handleBindingResponse(t *testing.T) {
	cl0 := Checklist{
		Pairs: Pairs{
			{
				Local: Candidate{
					Addr: Addr{
						Port: 10230,
						IP:   net.IPv4(10, 0, 0, 2),
					},
				},
				Remote: Candidate{
					Addr: Addr{
						Port: 31230,
						IP:   net.IPv4(10, 0, 0, 1),
					},
				},
				Foundation: []byte{1, 3},
				Priority:   1234,
			},
		},
	}
	a := &Agent{
		set: ChecklistSet{cl0},
	}
	mustInit(t, a)
	_, cID := a.nextChecklist()
	a.checklist = cID
	pID, err := a.pickPair()
	if err != nil {
		t.Fatal(err)
	}
	pair := a.set[a.checklist].Pairs[pID]
	at := &agentTransaction{
		id:        stun.NewTransactionID(),
		pair:      0,
		checklist: 0,
	}
	ctx := context{
		localUsername:  "LFRAG",
		remoteUsername: "RFRAG",
		remotePassword: "RPASS",
		localPassword:  "LPASS",
		localPref:      10,
	}
	a.ctx[pairContextKey(&pair)] = ctx
	integrity := stun.NewShortTermIntegrity(ctx.remotePassword)
	xorAddr := stun.XORMappedAddress{
		IP:   pair.Local.Addr.IP,
		Port: pair.Local.Addr.Port,
	}
	msg := stun.MustBuild(at.id, stun.BindingSuccess,
		stun.NewUsername("RFRAG:LFRAG"), &xorAddr,
		integrity, stun.Fingerprint,
	)
	if err := a.handleBindingResponse(at, &pair, msg, pair.Remote.Addr); err != nil {
		t.Fatal(err)
	}
	if len(a.set[0].Valid) == 0 {
		t.Error("valid set is empty")
	}
}

func TestAgent_check(t *testing.T) {
	a := Agent{}
	var c Checklist
	loadGoldenJSON(t, &c, "checklist.json")
	a.set = append(a.set, c)
	randSource := rand.NewSource(1)
	a.rand = rand.New(randSource)
	if err := a.init(); err != nil {
		t.Fatal(err)
	}
	if a.tiebreaker != 5721121980023635282 {
		t.Fatal(a.tiebreaker)
	}
	if a.role != Controlling {
		t.Fatal("bad role")
	}
	a.updateState()
	t.Logf("state: %s", a.state)
	pair := &a.set[0].Pairs[0]
	integrity := stun.NewShortTermIntegrity("RPASS")
	stunAgent := &stunMock{}
	xorAddr := &stun.XORMappedAddress{
		IP:   pair.Local.Addr.IP,
		Port: pair.Local.Addr.Port,
	}
	a.ctx[pairContextKey(pair)] = context{
		localUsername:  "LFRAG",
		remoteUsername: "RFRAG",
		remotePassword: "RPASS",
		localPassword:  "LPASS",
		stun:           stunAgent,
		localPref:      10,
	}
	t.Run("OK", func(t *testing.T) {
		checkMessage := func(t *testing.T, m *stun.Message) {
			t.Helper()
			if err := integrity.Check(m); err != nil {
				t.Errorf("failed to startCheck integrity: %v", err)
			}
			var u stun.Username
			if err := u.GetFrom(m); err != nil {
				t.Errorf("failed to get username: %v", err)
			}
			if u.String() != "RFRAG:LFRAG" {
				t.Errorf("unexpected username: %s", u)
			}
			var p PriorityAttr
			if err := p.GetFrom(m); err != nil {
				t.Error("failed to get priority attribute")
			}
			if p != 1845496575 {
				t.Errorf("unexpected priority: %d", p)
			}
		}
		t.Run("Controlling", func(t *testing.T) {
			var tid transactionID
			stunAgent.start = func(m *stun.Message) error {
				checkMessage(t, m)
				var (
					rControlling AttrControlling
					rControlled  AttrControlled
				)
				if rControlled.GetFrom(m) == nil {
					t.Error("unexpected controlled attribute")
				}
				if err := rControlling.GetFrom(m); err != nil {
					t.Error(err)
				}
				if rControlling != 5721121980023635282 {
					t.Errorf("unexpected tiebreaker: %d", rControlling)
				}
				tid = m.TransactionID
				return nil
			}
			if err := a.startCheck(pair); err != nil {
				t.Fatal("failed to startCheck", err)
			}
			resp := stun.MustBuild(stun.NewTransactionIDSetter(tid), stun.BindingSuccess, xorAddr, integrity, stun.Fingerprint)
			if err := a.processBindingResponse(pair, resp, pair.Remote.Addr); err != nil {
				t.Error(err)
			}
		})
		t.Run("Controlled", func(t *testing.T) {
			a.role = Controlled
			var tid transactionID
			stunAgent.start = func(m *stun.Message) error {
				checkMessage(t, m)
				var (
					rControlling AttrControlling
					rControlled  AttrControlled
				)
				if rControlling.GetFrom(m) == nil {
					t.Error("unexpected controlled attribute")
				}
				if err := rControlled.GetFrom(m); err != nil {
					t.Error(err)
				}
				if rControlled != 5721121980023635282 {
					t.Errorf("unexpected tiebreaker: %d", rControlled)
				}
				tid = m.TransactionID
				return nil
			}
			if err := a.startCheck(pair); err != nil {
				t.Fatal("failed to startCheck", err)
			}
			resp := stun.MustBuild(stun.NewTransactionIDSetter(tid), stun.BindingSuccess, xorAddr, integrity, stun.Fingerprint)
			if err := a.processBindingResponse(pair, resp, pair.Remote.Addr); err != nil {
				t.Error(err)
			}
		})
	})
	t.Run("STUN Agent failure", func(t *testing.T) {
		stunErr := errors.New("failed")
		stunAgent.start = func(m *stun.Message) error {
			return stunErr
		}
		if err := a.startCheck(pair); err != stunErr {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("STUN Unrecoverable error", func(t *testing.T) {
		var tid transactionID
		stunAgent.start = func(m *stun.Message) error {
			tid = m.TransactionID
			return nil
		}
		codeErr := unrecoverableErrorCodeErr{Code: stun.CodeBadRequest}
		if err := a.startCheck(pair); err != nil {
			t.Fatal(err)
		}
		resp := stun.MustBuild(stun.NewTransactionIDSetter(tid), stun.BindingError, stun.CodeBadRequest, integrity, stun.Fingerprint)
		if err := a.processBindingResponse(pair, resp, pair.Remote.Addr); err != codeErr {
			t.Fatalf("unexpected error %v", err)
		}
	})
	t.Run("STUN Error response without code", func(t *testing.T) {
		var tid transactionID
		stunAgent.start = func(m *stun.Message) error {
			tid = m.TransactionID
			return nil
		}
		if err := a.startCheck(pair); err != nil {
			t.Fatal(err)
		}
		resp := stun.MustBuild(tid, stun.BindingError, integrity, stun.Fingerprint)
		if err := a.processBindingResponse(pair, resp, pair.Remote.Addr); err == nil {
			t.Fatal("unexpected success")
		}
	})
	t.Run("STUN Role conflict", func(t *testing.T) {
		var tid transactionID
		stunAgent.start = func(m *stun.Message) error {
			tid = m.TransactionID
			return nil
		}
		resp := stun.MustBuild(tid, stun.BindingError, stun.CodeRoleConflict, xorAddr, integrity, stun.Fingerprint)
		if err := a.startCheck(pair); err != nil {
			t.Fatal(err)
		}
		if err := a.processBindingResponse(pair, resp, pair.Remote.Addr); err != errRoleConflict {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("STUN Integrity error", func(t *testing.T) {
		var tid transactionID
		stunAgent.start = func(m *stun.Message) error {
			tid = m.TransactionID
			return nil
		}
		if err := a.startCheck(pair); err != nil {
			t.Fatal(err)
		}
		i := stun.NewShortTermIntegrity("RPASS+BAD")
		resp := stun.MustBuild(tid, stun.BindingSuccess, i, xorAddr, stun.Fingerprint)
		if err := a.processBindingResponse(pair, resp, pair.Remote.Addr); err != stun.ErrIntegrityMismatch {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("STUN No fingerprint", func(t *testing.T) {
		var tid transactionID
		stunAgent.start = func(m *stun.Message) error {
			tid = m.TransactionID
			return nil
		}
		if err := a.startCheck(pair); err != nil {
			t.Fatal(err)
		}
		resp := stun.MustBuild(tid, stun.BindingSuccess, integrity)
		if err := a.processBindingResponse(pair, resp, pair.Remote.Addr); err != errFingerprintNotFound {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("STUN Bad fingerprint", func(t *testing.T) {
		var tid transactionID
		stunAgent.start = func(m *stun.Message) error {
			tid = m.TransactionID
			return nil
		}
		if err := a.startCheck(pair); err != nil {
			t.Fatal(err)
		}
		badFP := stun.RawAttribute{Type: stun.AttrFingerprint, Value: []byte{'b', 'a', 'd', 0}}
		resp := stun.MustBuild(tid, stun.BindingSuccess, integrity, badFP)
		if err := a.processBindingResponse(pair, resp, pair.Remote.Addr); err != stun.ErrFingerprintMismatch {
			t.Fatalf("unexpected error: %v", err)
		}
		t.Run("Should be done before integrity startCheck", func(t *testing.T) {
			var tid transactionID
			stunAgent.start = func(m *stun.Message) error {
				tid = m.TransactionID
				return nil
			}
			if err := a.startCheck(pair); err != nil {
				t.Fatal(err)
			}
			i := stun.NewShortTermIntegrity("RPASS+BAD")
			badFP := stun.RawAttribute{Type: stun.AttrFingerprint, Value: []byte{'b', 'a', 'd', 0}}
			resp := stun.MustBuild(tid, stun.BindingSuccess, i, badFP)
			if err := a.processBindingResponse(pair, resp, pair.Remote.Addr); err != stun.ErrFingerprintMismatch {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	})
	t.Run("STUN Wrong response message type", func(t *testing.T) {
		var tid transactionID
		stunAgent.start = func(m *stun.Message) error {
			tid = m.TransactionID
			return nil
		}
		typeErr := unexpectedResponseTypeErr{Type: stun.BindingRequest}
		if err := a.startCheck(pair); err != nil {
			t.Fatal(err)
		}
		resp := stun.MustBuild(tid, stun.BindingRequest, stun.CodeBadRequest, integrity, stun.Fingerprint)
		if err := a.processBindingResponse(pair, resp, pair.Remote.Addr); err != typeErr {
			t.Fatalf("unexpected success")
		}
	})
}

type candidateAndConn struct {
	Candidate Candidate
	Conn      net.PacketConn
}

func TestAgentAPI(t *testing.T) {
	// 0) Gather interfaces.
	addr, err := Gather()
	if err != nil {
		t.Fatal(err)
	}
	hostAddr, err := HostAddresses(addr)
	if err != nil {
		t.Error(err)
	}
	t.Logf("got host candidates: %d", len(hostAddr))
	for _, a := range hostAddr {
		t.Logf(" %s (%d)", a.IP, a.LocalPreference)
	}
	var toClose []io.Closer
	defer func() {
		for _, f := range toClose {
			if cErr := f.Close(); cErr != nil {
				t.Error(cErr)
			}
		}
	}()
	var local, remote Candidates
	for _, a := range hostAddr {
		l, r := newUDPCandidate(t, a), newUDPCandidate(t, a)
		toClose = append(toClose, l.Conn, r.Conn)
		local = append(local, l.Candidate)
		remote = append(remote, r.Candidate)
	}
	sort.Sort(local)
	sort.Sort(remote)
	list := new(Checklist)
	list.Pairs = NewPairs(local, remote)
	list.ComputePriorities(Controlling)
	list.Sort()
	list.Prune()
	t.Logf("got %d pairs", len(list.Pairs))
	for _, p := range list.Pairs {
		p.SetFoundation()
		t.Logf("%s -> %s [%x]", p.Local.Addr, p.Remote.Addr, p.Foundation)
	}
	if *writeGolden {
		saveGoldenJSON(t, list, "checklist.json")
	}
}

func TestAgent_nextChecklist(t *testing.T) {
	for _, tc := range []struct {
		Name    string
		Set     ChecklistSet
		ID      int
		Current int
	}{
		{
			Name:    "blank",
			ID:      noChecklist,
			Current: noChecklist,
		},
		{
			Name:    "first",
			Set:     ChecklistSet{{}},
			ID:      0,
			Current: noChecklist,
		},
		{
			Name:    "no running",
			Set:     ChecklistSet{{State: ChecklistFailed}},
			ID:      noChecklist,
			Current: noChecklist,
		},
		{
			Name:    "second",
			Set:     ChecklistSet{{}, {}},
			ID:      1,
			Current: 0,
		},
		{
			Name:    "second running",
			Set:     ChecklistSet{{}, {State: ChecklistFailed}, {}},
			ID:      2,
			Current: 0,
		},
		{
			Name:    "circle",
			Set:     ChecklistSet{{}, {State: ChecklistFailed}, {}},
			ID:      0,
			Current: 2,
		},
		{
			Name:    "circle without running",
			Set:     ChecklistSet{{State: ChecklistFailed}, {State: ChecklistFailed}},
			ID:      noChecklist,
			Current: 1,
		},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			a := &Agent{set: tc.Set, checklist: tc.Current}
			_, id := a.nextChecklist()
			if id != tc.ID {
				t.Errorf("nextChecklist %d (got) != %d (expected)", id, tc.ID)
			}
		})
	}
}

func TestAgent_pickPair(t *testing.T) {
	for _, tc := range []struct {
		Name      string
		Set       ChecklistSet
		Checklist int
		ID        int
		Err       error
	}{
		{
			Name:      "no checklist",
			Checklist: noChecklist,
			ID:        noPair,
			Err:       errNoChecklist,
		},
		{
			Name:      "no pair",
			Checklist: 0,
			ID:        noPair,
			Err:       errNoPair,
			Set:       ChecklistSet{{}},
		},
		{
			Name:      "first",
			Checklist: 0,
			ID:        0,
			Set: ChecklistSet{
				{Pairs: Pairs{{State: PairWaiting}}},
			},
		},
		{
			Name:      "all failed",
			Checklist: 0,
			ID:        noPair,
			Err:       errNoPair,
			Set: ChecklistSet{
				{Pairs: Pairs{{State: PairFailed}}},
			},
		},
		{
			Name:      "simple unfreeze",
			Checklist: 0,
			ID:        0,
			Set: ChecklistSet{
				{Pairs: Pairs{{State: PairFrozen}}},
			},
		},
		{
			Name:      "simple no unfreeze",
			Checklist: 0,
			ID:        1,
			Set: ChecklistSet{
				{Pairs: Pairs{
					{State: PairFrozen, Foundation: []byte{1}},
					{State: PairWaiting, Foundation: []byte{1}},
				}},
			},
		},
		{
			Name:      "no unfreeze from other checklist",
			Checklist: 1,
			ID:        noPair,
			Err:       errNoPair,
			Set: ChecklistSet{
				{Pairs: Pairs{
					{State: PairWaiting, Foundation: []byte{1}},
				}},
				{Pairs: Pairs{
					{State: PairFrozen, Foundation: []byte{1}},
				}},
			},
		},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			a := &Agent{set: tc.Set, checklist: tc.Checklist}
			id, err := a.pickPair()
			if err != tc.Err {
				t.Errorf("pickPair error %v (got) != %v (expected)", err, tc.Err)
			}
			if id != tc.ID {
				t.Errorf("pickPair id %d (got) != %d (expected)", id, tc.ID)
			}
		})
	}
	t.Run("first unfreeze only", func(t *testing.T) {
		a := &Agent{
			checklist: 0,
			set: ChecklistSet{
				{Pairs: Pairs{
					{State: PairFrozen, Foundation: []byte{1}},
					{State: PairFrozen, Foundation: []byte{2}},
				}},
			},
		}
		id, err := a.pickPair()
		if err != nil {
			t.Fatal(err)
		}
		if id != 0 {
			t.Error("bad pair picked")
		}
		if a.set[0].Pairs[1].State != PairFrozen {
			t.Error("second pair should be frozen")
		}
		if a.set[0].Pairs[0].State != PairInProgress {
			t.Error("first pair should be in progress")
		}
	})
}

func BenchmarkAgent_pickPair(b *testing.B) {
	b.Run("Simple", func(b *testing.B) {
		a := &Agent{
			set: ChecklistSet{{
				Pairs: Pairs{
					{
						Foundation: []byte{1, 2, 3, 100, 31, 22},
					},
				},
			}},
		}
		if err := a.init(); err != nil {
			b.Fatal(err)
		}
		_, checklist := a.nextChecklist()
		if checklist == noChecklist {
			b.Fatal("no checklist")
		}
		a.checklist = checklist

		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			id, err := a.pickPair()
			if err != nil {
				b.Fatal(err)
			}
			a.setPairState(a.checklist, id, PairWaiting)
		}
	})
	b.Run("Frozen", func(b *testing.B) {
		a := &Agent{
			checklist: 0,
			set: ChecklistSet{
				{
					Pairs: Pairs{
						{State: PairFrozen, Foundation: []byte{1, 2, 3, 100, 31, 22}},
						{State: PairFailed, Foundation: []byte{1, 2, 3, 100, 31, 22}},
						{State: PairFrozen, Foundation: []byte{1, 2, 3, 100, 31, 22}},
						{State: PairFrozen, Foundation: []byte{1, 2, 3, 100, 31, 24}},
						{State: PairFrozen, Foundation: []byte{1, 2, 3, 100, 31, 23}},
						{State: PairFrozen, Foundation: []byte{1, 2, 3, 100, 31, 22}},
					},
				},
				{
					Pairs: Pairs{
						{State: PairFrozen, Foundation: []byte{1, 2, 3, 100, 31, 21}},
						{State: PairFrozen, Foundation: []byte{1, 2, 3, 100, 31, 22}},
						{State: PairWaiting, Foundation: []byte{1, 2, 3, 100, 31, 21}},
						{State: PairFrozen, Foundation: []byte{1, 2, 3, 100, 31, 22}},
						{State: PairWaiting, Foundation: []byte{1, 2, 3, 100, 31, 23}},
						{State: PairFrozen, Foundation: []byte{1, 2, 3, 100, 31, 20}},
					},
				},
				{
					Pairs: Pairs{
						{State: PairFrozen, Foundation: []byte{1, 2, 3, 100, 31, 22}},
					},
				},
				{
					Pairs: Pairs{
						{State: PairFrozen, Foundation: []byte{1, 2, 3, 100, 31, 22}},
					},
				},

				{
					Pairs: Pairs{
						{State: PairFrozen, Foundation: []byte{1, 2, 3, 100, 31, 22}},
					},
				},
			},
		}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			id, err := a.pickPair()
			if err != nil {
				b.Fatal(err)
			}
			a.setPairState(a.checklist, id, PairFrozen)
		}
	})
}

func TestAgent_updateState(t *testing.T) {
	for _, tc := range []struct {
		Name  string
		State State
		Agent *Agent
	}{
		{
			Name:  "OneCompleted",
			State: Completed,
			Agent: &Agent{
				set: ChecklistSet{
					{State: ChecklistCompleted},
				},
			},
		},
		{
			Name:  "OneFailed",
			State: Failed,
			Agent: &Agent{
				set: ChecklistSet{
					{State: ChecklistFailed},
				},
			},
		},
		{
			Name:  "OneRunning",
			State: Running,
			Agent: &Agent{
				set: ChecklistSet{
					{State: ChecklistRunning},
				},
			},
		},
		{
			Name:  "OneCompletedOneRunning",
			State: Running,
			Agent: &Agent{
				set: ChecklistSet{
					{State: ChecklistRunning},
					{State: ChecklistCompleted},
				},
			},
		},
		{
			Name:  "OneFailedOneRunning",
			State: Running,
			Agent: &Agent{
				set: ChecklistSet{
					{State: ChecklistRunning},
					{State: ChecklistFailed},
				},
			},
		},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			tc.Agent.updateState()
			if tc.State != tc.Agent.state {
				t.Errorf("%s (got) != %s (expected)", tc.Agent.state, tc.State)
			}
		})
	}

}

func TestAgent_init(t *testing.T) {
	a := Agent{}
	var c Checklist
	loadGoldenJSON(t, &c, "checklist.json")
	a.set = append(a.set, c)
	if err := a.init(); err != nil {
		t.Fatal(err)
	}
	a.updateState()
	t.Logf("state: %s", a.state)
	if *writeGolden {
		saveGoldenJSON(t, a.set[0], "checklist_updated.json")
	}
	var cGolden Checklist
	loadGoldenJSON(t, &cGolden, "checklist_updated.json")
	if !cGolden.Equal(a.set[0]) {
		t.Error("got unexpected checklist after init")
	}
}

func BenchmarkPairContextKey(b *testing.B) {
	p := Pair{
		Local: Candidate{
			Addr: Addr{
				IP:    net.IPv4(127, 0, 0, 1),
				Port:  31223,
				Proto: candidate.UDP,
			},
		},
		Remote: Candidate{
			Addr: Addr{
				IP:    net.IPv4(127, 0, 0, 1),
				Port:  31223,
				Proto: candidate.UDP,
			},
		},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		k := pairContextKey(&p)
		if k.LocalPort == 0 {
			b.Fatal("bad port")
		}
	}
}

func shouldNotAllocate(t *testing.T, f func()) {
	t.Helper()
	if a := testing.AllocsPerRun(10, f); a > 0 {
		t.Errorf("unexpected allocations: %f", a)
	}
}

func TestFoundationSet(t *testing.T) {
	t.Run("Add", func(t *testing.T) {
		fs := make(foundationSet)
		shouldNotAllocate(t, func() {
			fs.Add([]byte{1, 2})
		})
		if !fs.Contains([]byte{1, 2}) {
			t.Error("does not contain {1, 2}")
		}
	})
	t.Run("Contains", func(t *testing.T) {
		fs := make(foundationSet)
		fs.Add([]byte{1, 2})
		if !fs.Contains([]byte{1, 2}) {
			t.Error("does not contain {1, 2}")
		}
		shouldNotAllocate(t, func() {
			fs.Contains([]byte{1, 2})
		})
		if fs.Contains([]byte{1, 3}) {
			t.Error("should not contain {1, 3}")
		}
	})
	t.Run("Panic on too big foundation", func(t *testing.T) {
		fs := make(foundationSet)
		f := make([]byte, 200)
		t.Run("Contains", func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("no panic")
				}
			}()
			fs.Contains(f)
		})
		t.Run("Add", func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("no panic")
				}
			}()
			fs.Add(f)
		})
	})
}
