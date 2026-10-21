/*
===========================================================================

monsterapproach_test.go - the +0x114 pursuit admission for dynamic spawns

A spawn that resolves its tactics from the spawn table alone has no
FleeType and no BattleStyle, and native installs the pursuit callback for
it as well (0x0053FC00 at 0x0053FC67). The port must admit those spawns,
and must not admit a nest that only looks like one.

===========================================================================
*/
package simulation

import (
	"testing"

	"opensro.online/server/internal/game/world/instance"
	"opensro.online/server/internal/game/world/monster"
)

/*
================
TestOrdinarySquadApproachAdmitsDetachedPolicySpawns

0x0053FC00 installs CAITactics_PursueTargetWithDirectionGate (0x00548BE0)
in slot +0x114 whenever FleeType and BattleStyle are zero. A dynamic spawn
carries neither a TacticsControls row nor a summoner, so the two fields
stay zero and the callback belongs to it.
================
*/
func TestOrdinarySquadApproachAdmitsDetachedPolicySpawns(t *testing.T) {
	questSpawn := monster.Instance{
		NestDetached: true,
		Nest:         monster.NestRow{PolicyPinned: true},
	}
	for name, actor := range map[string]monster.Instance{
		"scripted spawn": questSpawn,
		"GM spawn":       questSpawn,
	} {
		if !ordinarySquadApproach(actor) {
			t.Fatalf("%s excluded from ordinarySquadApproach (+0x114 0x00548BE0)", name)
		}
	}
}

/*
================
TestOrdinarySquadApproachKeepsUnpinnedAndAttachedNestsOut

Native resolves a monster's tactics once, when it is spawned, so a
monster that loses its nest slot later (53FD40) keeps the tactics it came
with and stays admitted. What never resolved a spawn table row stays out:
a nest that was never pinned (the stuck path) and a resource-authored area
nest that still holds its slot.
================
*/
func TestOrdinarySquadApproachKeepsUnpinnedAndAttachedNestsOut(t *testing.T) {
	for name, actor := range map[string]monster.Instance{
		"detached but unpinned nest": {NestDetached: true},
		"attached area nest":         {Nest: monster.NestRow{PolicyPinned: true}},
		"plain instance":             {},
	} {
		if ordinarySquadApproach(actor) {
			t.Fatalf("%s unexpectedly admitted to ordinarySquadApproach", name)
		}
	}
}

/*
================
TestQuestAndGMSpawnsJoinTheOrdinaryPursuit

A quest script (0x008B9B60, 0x008B2AB0, 0x008B6840, 0x008C7910) and
/LOADMONSTER (0x00520A40) both spawn without a tactics id, so the server
resolves the spawn table row itself. The spawn table rows of the four
quest monsters carry no control block, so the pursuit belongs to them.
================
*/
func TestQuestAndGMSpawnsJoinTheOrdinaryPursuit(t *testing.T) {
	const guardian = "MOB_QT_02_PUNISHER_CLON"
	state := NewMonsterState(monster.TemplateFromParts(map[uint32]monster.MonsterRef{
		1: {RefObjID: 1, Codename: guardian, TidWord: 0xc6, MaxHP: 100, WalkSpeed: 8, RunSpeed: 20},
	}, nil))
	state.SetRandomSource(func() float64 { return 0 })
	state.StartDivision("quest")
	lease, exists := state.PopulationLease("quest", instance.Pack(1, 1))
	if !exists {
		t.Fatal("ordinary population absent")
	}
	if !state.SpawnQuestMonster(QuestMonsterSpawn{Division: "quest", Population: lease, Codename: guardian,
		Position: Spawn{RegionID: 0x655e, X: 253, Y: 85, Z: 449}, NowMs: 1000, RadiusSpan: 20}) {
		t.Fatal("guardian admission failed")
	}
	actors := state.MaterializedInstances("quest")
	if len(actors) != 1 {
		t.Fatalf("quest population %d, want 1", len(actors))
	}
	if actors[0].Nest.HasControls {
		t.Fatal("the guardian's spawn table row grew a control block")
	}
	if !ordinarySquadApproach(actors[0]) {
		t.Fatal("the quest guardian is excluded from the +0x114 pursuit")
	}

	gm, gmLease := gmMonsterState(t, 0)
	if gm.SpawnGMMonsters(GMMonsterSpawn{Division: "gm", Population: gmLease, RefObjID: 1, Count: 1,
		Position: Spawn{RegionID: 0x655e, X: 253, Y: 85, Z: 449}, NowMs: 1000}) != 1 {
		t.Fatal("GM spawn refused")
	}
	loaded := gm.MaterializedInstances("gm")
	if len(loaded) != 1 {
		t.Fatalf("GM population %d, want 1", len(loaded))
	}
	if !ordinarySquadApproach(loaded[0]) {
		t.Fatal("the /LOADMONSTER spawn is excluded from the +0x114 pursuit")
	}
}
