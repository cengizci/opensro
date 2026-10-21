/*
===========================================================================

honor_test.go - Academy Honor lifecycle, graduation, kick/leave, disband,
and 7-day reopen cooldown over the real transport

===========================================================================
*/

package mentor_test

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"opensro.online/server/internal/game/enterworld"
	"opensro.online/server/internal/game/item/wire"
	"opensro.online/server/internal/game/social/mentor"
	"opensro.online/server/internal/testsupport/wait"
)

/*
================
campGraduateGrant

The graduate EXP grant the mentor runtime hands to the world through
GrantGraduateExperience. The grant is produced on the session goroutine
and read on the test goroutine, so it travels over a channel: a shared
variable would be a data race, and reading it behind an unrelated frame
would be a timing bug.
================
*/
type campGraduateGrant struct {
	charID int64
	exp    int64
}

/*
================
campRankRecorder

Records the mentor runtime's honor-rank syncs. The runtime calls the
hook from a session goroutine, so the recorder owns the channel and the
test drains it between phases; a shared slice would be a data race.
================
*/
type campRankRecorder struct {
	ranks chan uint8
}

/*
==================
newCampRankRecorder
==================
*/
func newCampRankRecorder() *campRankRecorder {
	return &campRankRecorder{ranks: make(chan uint8, 64)}
}

/*
================
record
================
*/
func (r *campRankRecorder) record(evaluationRank uint8) {
	r.ranks <- evaluationRank
}

/*
================
drain

Removes every recorded rank and returns them in arrival order. The test
drains before a phase whose result it wants to observe on its own, so a
late sync from an earlier phase cannot be mistaken for the phase's own
sync.
================
*/
func (r *campRankRecorder) drain() []uint8 {
	var out []uint8
	for {
		select {
		case rank := <-r.ranks:
			out = append(out, rank)
		default:
			return out
		}
	}
}

/*
================
campClock

The mentor runtime's clock. The runtime reads it from its session
goroutines while the test advances it between phases, so the value lives
in an atomic counter instead of a shared variable.
================
*/
type campClock struct {
	nanos atomic.Int64
}

/*
==================
newCampClock
==================
*/
func newCampClock(start time.Time) *campClock {
	c := &campClock{}
	c.nanos.Store(start.UnixNano())
	return c
}

/*
================
now
================
*/
func (c *campClock) now() time.Time {
	return time.Unix(0, c.nanos.Load()).UTC()
}

/*
================
advance
================
*/
func (c *campClock) advance(step time.Duration) {
	c.nanos.Add(int64(step))
}

type stubLevelData map[int64]int64

func (s stubLevelData) ExpRequired(level int64) (int64, bool) {
	v, ok := s[level]
	return v, ok
}

func (s stubLevelData) MonsterExpBasis(level int64) (int64, bool) {
	v, ok := s[level]
	return v, ok
}

func (s stubLevelData) SkillPointCost(level int64) (int64, bool) {
	v, ok := s[level]
	return v, ok
}

/*
================
TestAcademyHonorLifecycleGraduationRankKickLeaveDisband
================
*/
func TestAcademyHonorLifecycleGraduationRankKickLeaveDisband(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "authority")
	race := func() *int64 { v := enterworld.RaceChina; return &v }
	gender := func() *int64 { v := enterworld.GenderMale; return &v }
	gold := int64(500000)

	seeds := []*enterworld.Character{
		{Name: campE2ENameM, ModelCodename: "CHAR_CH_MAN_ADVENTURER", RaceIndex: race(), Gender: gender(), Level: campLevel(60), Gold: &gold},
		{Name: campE2ENameS, ModelCodename: "CHAR_CH_MAN_ADVENTURER", RaceIndex: race(), Gender: gender(), Level: campLevel(1)},
		{Name: campE2ENameO, ModelCodename: "CHAR_CH_MAN_ADVENTURER", RaceIndex: race(), Gender: gender(), Level: campLevel(20)},
		{Name: campE2ENameI, ModelCodename: "CHAR_CH_MAN_ADVENTURER", RaceIndex: race(), Gender: gender(), Level: campLevel(20)},
	}
	env := startCampServer(t, dir, seeds)

	clock := newCampClock(time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC))
	env.mentorInv.Now = clock.now
	env.mentorInv.LevelData = stubLevelData{40: 1473482}

	grants := make(chan campGraduateGrant, 16)
	env.mentorInv.GrantGraduateExperience = func(_ string, charID int64, expDelta int64) {
		grants <- campGraduateGrant{charID: charID, exp: expDelta}
	}
	ranks := newCampRankRecorder()
	env.mentorInv.SyncMasterHonorBuffs = func(_ string, _ int64, evaluationRank uint8) {
		ranks.record(evaluationRank)
	}

	wsM := campDialWS(t, env.srv)
	campHelloWS(t, wsM)
	campEnterWorld(t, wsM, campE2ENameM)

	wsS := campDialWS(t, env.srv)
	campHelloWS(t, wsS)
	campEnterWorld(t, wsS, campE2ENameS)

	wsO := campDialWS(t, env.srv)
	campHelloWS(t, wsO)
	campEnterWorld(t, wsO, campE2ENameO)

	wsI := campDialWS(t, env.srv)
	campHelloWS(t, wsI)
	campEnterWorld(t, wsI, campE2ENameI)

	charM := campE2ECharacter(t, env.authority, campE2ENameM)
	charS := campE2ECharacter(t, env.authority, campE2ENameS)
	charO := campE2ECharacter(t, env.authority, campE2ENameO)
	charI := campE2ECharacter(t, env.authority, campE2ENameI)
	gidM := enterworld.ObjectIDForCharacter(charM)
	gidS := enterworld.ObjectIDForCharacter(charS)
	gidO := enterworld.ObjectIDForCharacter(charO)
	gidI := enterworld.ObjectIDForCharacter(charI)

	// 1. Master opens camp (0x72EC -> 0xB2EC + 0x30B3).
	campSendFrame(t, wsM, mentor.OpTCOpenRequest, campU32(0))
	openAck := campExpectFrame(t, wsM, mentor.OpTCOpenAnswer, "open ack")
	if len(openAck) == 0 || openAck[0] != 1 {
		t.Fatalf("0xB2EC open ack = % X, want status 1", openAck)
	}
	campExpectFrame(t, wsM, wire.OpPointsUpdate, "open fee gold refresh")

	// 2. Invite S (joined at level 1), O (joined at level 20), and I (joined at level 20).
	for _, target := range []struct {
		gid uint32
		ws  *websocket.Conn
	}{
		{gidS, wsS},
		{gidO, wsO},
		{gidI, wsI},
	} {
		campSendFrame(t, wsM, mentor.OpTCInviteRequest, campU32(target.gid))
		campExpectFrame(t, target.ws, 0x3393, "invite prompt")
		campSendFrame(t, target.ws, 0x3393, []byte{1, 1})
		campExpectFrame(t, target.ws, mentor.OpTCStatus, "joiner camp seed")
		campExpectFrame(t, wsM, mentor.OpTCStatus, "master join push")
	}
	// Drain cross-student join pushes on S and O.
	campExpectFrame(t, wsS, mentor.OpTCStatus, "S sees O join")
	campExpectFrame(t, wsS, mentor.OpTCStatus, "S sees I join")
	campExpectFrame(t, wsO, mentor.OpTCStatus, "O sees I join")

	// 2b. Before 24h has elapsed since join:
	//   - Student I trying to leave (0x7785) receives 0xB785 {2, 0x21} (ErrTCNotOut24h).
	//   - Master M trying to kick I (0x74D4) receives 0xB4D4 {2, 0x20} (ErrTCNotBan24h).
	//   - Student I trying to kick O (0x74D4) receives 0xB4D4 {2, 0x16} (ErrTCNotCancel).
	campSendFrame(t, wsI, mentor.OpTCLeaveRequest, nil)
	if got := campExpectFrame(t, wsI, mentor.OpTCLeaveAnswer, "early leave 24h refusal"); !bytes.Equal(got, []byte{2, mentor.ErrTCNotOut24h}) {
		t.Fatalf("early 0xB785 = % X, want [02 %02X]", got, mentor.ErrTCNotOut24h)
	}
	campSendFrame(t, wsM, mentor.OpTCKickRequest, campStr(charI.Name))
	if got := campExpectFrame(t, wsM, mentor.OpTCKickAnswer, "early kick 24h refusal"); !bytes.Equal(got, []byte{2, mentor.ErrTCNotBan24h}) {
		t.Fatalf("early 0xB4D4 = % X, want [02 %02X]", got, mentor.ErrTCNotBan24h)
	}
	campSendFrame(t, wsI, mentor.OpTCKickRequest, campStr(charO.Name))
	if got := campExpectFrame(t, wsI, mentor.OpTCKickAnswer, "non-master kick refusal"); !bytes.Equal(got, []byte{2, mentor.ErrTCNotCancel}) {
		t.Fatalf("non-master 0xB4D4 = % X, want [02 %02X]", got, mentor.ErrTCNotCancel)
	}

	// 3. Student S reaches level 40 via deps.Update (triggering OnCharacterLevelChanged ->
	// 0x3AC5 [5][gidS][2][40] to all online camp members) and graduates via 0x710A
	// (satisfaction=4, Very Satisfied: +5 eval pts, +39 honor pts).
	env.deps.Update(charS, "test-level-40", func() bool {
		lvl := int64(40)
		charS.Level = &lvl
		return true
	})
	wantLvl40Frame := mentor.EncodeCampMemberLevelUpdate3AC5(gidS, 40)
	for _, peer := range []*websocket.Conn{wsM, wsS, wsO, wsI} {
		if got := campExpectFrame(t, peer, mentor.OpTCStatus, "sees S level 40"); !bytes.Equal(got, wantLvl40Frame) {
			t.Fatalf("0x3AC5 level 40 = % X, want % X", got, wantLvl40Frame)
		}
	}
	clock.advance(25 * time.Hour)
	campSendFrame(t, wsS, mentor.OpTCGraduateRequest, []byte{4})
	if got := campExpectFrame(t, wsS, mentor.OpTCGraduateAnswer, "S graduate ack"); !bytes.Equal(got, []byte{1}) {
		t.Fatalf("0xB10A = % X, want [01]", got)
	}
	if got := campExpectFrame(t, wsS, mentor.OpTCStatus, "S graduate status-4"); !bytes.Equal(got, mentor.EncodeCampMemberGraduate3AC5(gidS, 0)) {
		t.Fatalf("S 0x3AC5 graduate = % X", got)
	}
	// The EXP grant is a separate step of the graduation, not part of the
	// status-4 frame. Wait for it instead of reading a shared variable
	// behind an unrelated frame.
	var grant campGraduateGrant
	wait.Eventually(t, 5*time.Second, "the graduate EXP grant to arrive", func() bool {
		select {
		case grant = <-grants:
			return true
		default:
			return false
		}
	})
	if grant.charID != charS.ID || grant.exp != mentor.GraduateBonusExp {
		t.Fatalf("graduate exp grant = (char %d, exp %d), want (%d, %d)", grant.charID, grant.exp, charS.ID, mentor.GraduateBonusExp)
	}
	// Master receives status-4 graduate, status-5 flag-3 honor points update (39 pts for joinLevel 1), and status-12 eval rank 1 (ranking 1 <= 5).
	if got := campExpectFrame(t, wsM, mentor.OpTCStatus, "M sees S graduate"); !bytes.Equal(got, mentor.EncodeCampMemberGraduate3AC5(gidS, 0)) {
		t.Fatalf("M 0x3AC5 graduate = % X", got)
	}
	if got := campExpectFrame(t, wsM, mentor.OpTCStatus, "M sees honor update"); !bytes.Equal(got, mentor.EncodeCampMemberHonorUpdate3AC5(gidM, 39)) {
		t.Fatalf("M 0x3AC5 honor update = % X", got)
	}
	if got := campExpectFrame(t, wsM, mentor.OpTCStatus, "M sees eval rank 1"); !bytes.Equal(got, mentor.EncodeCampEvaluationUpdate3AC5(1)) {
		t.Fatalf("M 0x3AC5 eval rank = % X", got)
	}
	// O and I receive status-4 graduate, status-5 flag-3 honor update, and status-12 eval rank 1.
	for _, peer := range []*websocket.Conn{wsO, wsI} {
		if got := campExpectFrame(t, peer, mentor.OpTCStatus, "peer sees S graduate"); !bytes.Equal(got, mentor.EncodeCampMemberGraduate3AC5(gidS, 0)) {
			t.Fatalf("peer 0x3AC5 graduate = % X", got)
		}
		if got := campExpectFrame(t, peer, mentor.OpTCStatus, "peer sees honor update"); !bytes.Equal(got, mentor.EncodeCampMemberHonorUpdate3AC5(gidM, 39)) {
			t.Fatalf("peer 0x3AC5 honor update = % X", got)
		}
		if got := campExpectFrame(t, peer, mentor.OpTCStatus, "peer sees eval rank 1"); !bytes.Equal(got, mentor.EncodeCampEvaluationUpdate3AC5(1)) {
			t.Fatalf("peer 0x3AC5 eval rank = % X", got)
		}
	}

	// Campless S trying kick (0x74D4), leave (0x7785), or disband (0x77C5) receives ErrTCNotInCamp (0x0B).
	campSendFrame(t, wsS, mentor.OpTCKickRequest, campStr(charO.Name))
	if got := campExpectFrame(t, wsS, mentor.OpTCKickAnswer, "campless kick refusal"); !bytes.Equal(got, []byte{2, mentor.ErrTCNotInCamp}) {
		t.Fatalf("campless 0xB4D4 = % X, want [02 %02X]", got, mentor.ErrTCNotInCamp)
	}
	campSendFrame(t, wsS, mentor.OpTCLeaveRequest, nil)
	if got := campExpectFrame(t, wsS, mentor.OpTCLeaveAnswer, "campless leave refusal"); !bytes.Equal(got, []byte{2, mentor.ErrTCNotInCamp}) {
		t.Fatalf("campless 0xB785 = % X, want [02 %02X]", got, mentor.ErrTCNotInCamp)
	}
	campSendFrame(t, wsS, mentor.OpTCDisbandRequest, nil)
	if got := campExpectFrame(t, wsS, mentor.OpTCDisbandAnswer, "campless disband refusal"); !bytes.Equal(got, []byte{2, mentor.ErrTCNotInCamp}) {
		t.Fatalf("campless 0xB7C5 = % X, want [02 %02X]", got, mentor.ErrTCNotInCamp)
	}

	// Verify HonorPoints and SpendHonorPoints hooks on InviteRuntime.
	if pts := env.mentorInv.HonorPoints(campE2EDivision, charM); pts != 39 {
		t.Fatalf("HonorPoints = %d, want 39", pts)
	}
	if !env.mentorInv.SpendHonorPoints(campE2EDivision, charM, 15) {
		t.Fatal("SpendHonorPoints(15) = false, want true")
	}
	if pts := env.mentorInv.HonorPoints(campE2EDivision, charM); pts != 24 {
		t.Fatalf("HonorPoints after spend = %d, want 24", pts)
	}
	// Drain the status-5 flag-3 spend broadcast on M, O, I.
	for _, peer := range []*websocket.Conn{wsM, wsO, wsI} {
		if got := campExpectFrame(t, peer, mentor.OpTCStatus, "spend honor push"); !bytes.Equal(got, mentor.EncodeCampMemberHonorUpdate3AC5(gidM, 24)) {
			t.Fatalf("spend 0x3AC5 = % X", got)
		}
	}

	// 4. Honor Rank leaderboard query (0x72D0 -> 0xB2D0).
	campSendFrame(t, wsM, mentor.OpTCHonorRankRequest, campU32(0))
	rankAns := campExpectFrame(t, wsM, mentor.OpTCHonorRankAnswer, "0xB2D0 leaderboard")
	if len(rankAns) < 2 || rankAns[0] != 1 || rankAns[1] != 1 {
		t.Fatalf("0xB2D0 = % X, want status=1 and count=1", rankAns)
	}

	// 5. Compulsory graduation (0x736D -> 0xB36D):
	//   - At level 40 (< 41), Master receives 0xB36D {2, 0x22} (ErrTCPossible41L).
	//   - At level 41 (with >24h elapsed), Master succeeds with 0xB36D {1}.
	env.authority.MutateCharacter(charO, "test-o-level-40", func() {
		lvl := int64(40)
		charO.Level = &lvl
	})
	campSendFrame(t, wsM, mentor.OpTCCompulsoryGraduateRequest, campStr(charO.Name))
	if got := campExpectFrame(t, wsM, mentor.OpTCCompulsoryGraduateAnswer, "0xB36D level 40 refusal"); !bytes.Equal(got, []byte{2, mentor.ErrTCPossible41L}) {
		t.Fatalf("level-40 0xB36D = % X, want [02 %02X]", got, mentor.ErrTCPossible41L)
	}
	env.authority.MutateCharacter(charO, "test-o-level-41", func() {
		lvl := int64(41)
		charO.Level = &lvl
	})
	campSendFrame(t, wsM, mentor.OpTCCompulsoryGraduateRequest, campStr(charO.Name))
	if got := campExpectFrame(t, wsM, mentor.OpTCCompulsoryGraduateAnswer, "0xB36D ack"); !bytes.Equal(got, []byte{1}) {
		t.Fatalf("0xB36D = % X, want [01]", got)
	}
	// M, O, I receive status-4 flag-1 compulsory graduate; M and I receive status-5 flag-3 honor update (24 + 20 = 44).
	for _, peer := range []*websocket.Conn{wsM, wsO, wsI} {
		if got := campExpectFrame(t, peer, mentor.OpTCStatus, "sees O compulsory grad"); !bytes.Equal(got, mentor.EncodeCampMemberGraduate3AC5(gidO, 1)) {
			t.Fatalf("0x3AC5 compulsory grad = % X", got)
		}
	}
	for _, peer := range []*websocket.Conn{wsM, wsI} {
		if got := campExpectFrame(t, peer, mentor.OpTCStatus, "sees M honor 44"); !bytes.Equal(got, mentor.EncodeCampMemberHonorUpdate3AC5(gidM, 44)) {
			t.Fatalf("0x3AC5 honor 44 = % X", got)
		}
	}

	// 6. Student I leaves voluntarily after >24h (0x7785 -> 0xB785 + 0x3AC5 status-3 flag-1).
	campSendFrame(t, wsI, mentor.OpTCLeaveRequest, nil)
	if got := campExpectFrame(t, wsI, mentor.OpTCLeaveAnswer, "0xB785 ack"); !bytes.Equal(got, []byte{1}) {
		t.Fatalf("0xB785 = % X, want [01]", got)
	}
	if got := campExpectFrame(t, wsI, mentor.OpTCStatus, "I sees self leave"); !bytes.Equal(got, mentor.EncodeCampMemberLeave3AC5(gidI, 1)) {
		t.Fatalf("I 0x3AC5 leave = % X", got)
	}
	if got := campExpectFrame(t, wsM, mentor.OpTCStatus, "M sees I leave"); !bytes.Equal(got, mentor.EncodeCampMemberLeave3AC5(gidI, 1)) {
		t.Fatalf("M 0x3AC5 I leave = % X", got)
	}

	// 7. Re-invite I, advance 25h so 24h join cooldown passes, and kick I (0x74D4 -> 0xB4D4 + 0x3AC5 status-3 flag-2).
	campSendFrame(t, wsM, mentor.OpTCInviteRequest, campU32(gidI))
	campExpectFrame(t, wsI, 0x3393, "re-invite I")
	campSendFrame(t, wsI, 0x3393, []byte{1, 1})
	campExpectFrame(t, wsI, mentor.OpTCStatus, "I re-join seed")
	campExpectFrame(t, wsM, mentor.OpTCStatus, "M sees I re-join")

	clock.advance(25 * time.Hour)
	campSendFrame(t, wsM, mentor.OpTCKickRequest, campStr(charI.Name))
	if got := campExpectFrame(t, wsM, mentor.OpTCKickAnswer, "0xB4D4 ack"); !bytes.Equal(got, []byte{1}) {
		t.Fatalf("0xB4D4 = % X, want [01]", got)
	}
	if got := campExpectFrame(t, wsM, mentor.OpTCStatus, "M sees I kicked"); !bytes.Equal(got, mentor.EncodeCampMemberLeave3AC5(gidI, 2)) {
		t.Fatalf("M 0x3AC5 kick = % X", got)
	}
	if got := campExpectFrame(t, wsI, mentor.OpTCStatus, "I sees self kicked"); !bytes.Equal(got, mentor.EncodeCampMemberLeave3AC5(gidI, 2)) {
		t.Fatalf("I 0x3AC5 kick = % X", got)
	}

	// 8. Master disband before 7 days (50h elapsed since creation) refuses with
	// 0xB7C5 {2, 0x1D, u32 remainingSeconds} (ErrTCDisbandWait7d).
	campSendFrame(t, wsM, mentor.OpTCDisbandRequest, nil)
	earlyDisbandAns := campExpectFrame(t, wsM, mentor.OpTCDisbandAnswer, "7-day disband cooldown")
	if len(earlyDisbandAns) != 6 || earlyDisbandAns[0] != 2 || earlyDisbandAns[1] != mentor.ErrTCDisbandWait7d {
		t.Fatalf("early 0xB7C5 = % X, want {2, 0x1D, u32}", earlyDisbandAns)
	}
	if gotRem := binary.LittleEndian.Uint32(earlyDisbandAns[2:6]); gotRem != uint32(7*24*3600-50*3600) {
		t.Fatalf("early 0xB7C5 remainingSeconds = %d, want %d", gotRem, 7*24*3600-50*3600)
	}

	// Advance past 7 days since creation: Master disbands camp (0x77C5 -> 0xB7C5 + 0x3AC5 status-1)
	// and hits 7-day reopen cooldown on 0x72EC.
	clock.advance(6 * 24 * time.Hour)
	campSendFrame(t, wsM, mentor.OpTCDisbandRequest, nil)
	if got := campExpectFrame(t, wsM, mentor.OpTCDisbandAnswer, "0xB7C5 ack"); !bytes.Equal(got, []byte{1}) {
		t.Fatalf("0xB7C5 = % X, want [01]", got)
	}
	if got := campExpectFrame(t, wsM, mentor.OpTCStatus, "M sees camp dissolved"); !bytes.Equal(got, mentor.EncodeCampDisband3AC5()) {
		t.Fatalf("M 0x3AC5 dissolve = % X, want [01]", got)
	}
	// The disband resets the master's honor-rank buff, so the runtime syncs
	// rank 0 for him. Wait for that sync: it lands on the session goroutine.
	wait.Eventually(t, 5*time.Second, "the disband's honor-rank sync to reach rank 0", func() bool {
		for _, rank := range ranks.drain() {
			if rank == 0 {
				return true
			}
		}
		return false
	})

	// Attempting to reopen 1 hour later answers 0xB2EC {2, 0x1C, remainingSeconds}.
	clock.advance(1 * time.Hour)
	campSendFrame(t, wsM, mentor.OpTCOpenRequest, campU32(0))
	waitAns := campExpectFrame(t, wsM, mentor.OpTCOpenAnswer, "7-day reopen cooldown")
	if len(waitAns) != 6 || waitAns[0] != 2 || waitAns[1] != mentor.OpenErrReopenWait {
		t.Fatalf("reopen wait 0xB2EC = % X, want {2, 0x1C, u32}", waitAns)
	}
	remSec := binary.LittleEndian.Uint32(waitAns[2:6])
	wantSec := uint32(7*24*3600 - 3600)
	if remSec != wantSec {
		t.Fatalf("remainingSeconds = %d, want %d", remSec, wantSec)
	}
}
