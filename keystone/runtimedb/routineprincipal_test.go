package runtimedb

import "testing"

func TestRoutineResourceSignatureTreatsOmittedAndEmptyListsEqually(t *testing.T) {
	omitted, omittedSignature, err := normalizeRoutineResources(RoutineResources{MemoryIDs: []int64{1}})
	if err != nil {
		t.Fatal(err)
	}
	empty, emptySignature, err := normalizeRoutineResources(RoutineResources{SecretIDs: []string{}, MemoryIDs: []int64{1}})
	if err != nil {
		t.Fatal(err)
	}
	if omittedSignature != emptySignature || len(omitted.SecretIDs) != len(empty.SecretIDs) {
		t.Fatalf("same resource selection produced different signatures: %q != %q", omittedSignature, emptySignature)
	}
}

func approvedResourceRoutine(t *testing.T, db *DB, resources RoutineResources, audience string) (string, *RoutinePrincipal) {
	t.Helper()
	if _, err := db.CreateHouseholdRoutine(HouseholdRoutine{Name: "resource-run", Ownership: RoutineWildstone, Author: "sam", Document: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AssignRoutineButler("resource-run", "shared", "mika"); err != nil {
		t.Fatal(err)
	}
	id, err := db.SubmitRoutinePlan("resource-run", "sam", "", audience, "household notification", "fixture", resources)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ApproveRoutinePlan(id, "mika"); err != nil {
		t.Fatal(err)
	}
	p, err := db.BindRoutinePrincipal(id)
	if err != nil {
		t.Fatal(err)
	}
	return id, p
}

func TestRoutineCannotUseASecretGrantedOnlyToItsProposer(t *testing.T) {
	db, vault, value := secretFixture(t)
	s, err := vault.Create("mika", SecretInput{Scope: "household", Label: "Calendar", Value: value})
	if err != nil {
		t.Fatal(err)
	}
	if err = vault.AgentGrant("mika", s.ID, "shared", "calendar.get_agenda", true); err != nil {
		t.Fatal(err)
	}
	id, routine := approvedResourceRoutine(t, db, RoutineResources{}, AudienceHousehold)
	_ = id
	for _, person := range []string{"sam", "mika"} {
		p, _ := db.GetButlerMember(person)
		if err = vault.Grant("mika", s.ID, SecretGrant{PersonID: p.PersonID, Verb: "use"}, true); err != nil {
			t.Fatal(err)
		}
		if err = vault.UseRoutine(s.ID, routine, "calendar.get_agenda", func([]byte) error { t.Fatal("borrowed Person grant"); return nil }); err == nil {
			t.Fatalf("routine borrowed %s's secret grant", person)
		}
	}
	personal, err := vault.Create("sam", SecretInput{Scope: "personal", Label: "Private", Value: value})
	if err != nil {
		t.Fatal(err)
	}
	if err = vault.AgentGrant("sam", personal.ID, "shared", "calendar.get_agenda", true); err != nil {
		t.Fatal(err)
	}
	if err = vault.UseRoutine(personal.ID, routine, "calendar.get_agenda", func([]byte) error { t.Fatal("Personal secret reached routine"); return nil }); err == nil {
		t.Fatal("unattended Personal secret use allowed")
	}
	if _, err = vault.RevealRoutine(s.ID, routine); err == nil {
		t.Fatal("routine revealed raw value")
	}
}

func TestUnattendedPersonalSecretUseIsAlwaysDenied(t *testing.T) {
	db, vault, value := secretFixture(t)
	personal, err := vault.Create("sam", SecretInput{Scope: "personal", Label: "Private", Value: value})
	if err != nil {
		t.Fatal(err)
	}
	if err = vault.AgentGrant("sam", personal.ID, "shared", "calendar.get_agenda", true); err != nil {
		t.Fatal(err)
	}
	interactive := db.BindMemoryRead("sam", "shared", MemoryDestination{BuiltIn: true})
	if err = vault.Use(personal.ID, interactive, "calendar.get_agenda", func([]byte) error { return nil }); err != nil {
		t.Fatalf("Person's interactive grant was not live: %v", err)
	}
	_, routine := approvedResourceRoutine(t, db, RoutineResources{}, AudienceHousehold)
	if err = vault.UseRoutine(personal.ID, routine, "calendar.get_agenda", func([]byte) error { t.Fatal("Personal value reached unattended run"); return nil }); err == nil {
		t.Fatal("Owner approval and shared Agent laundered Personal secret use")
	}
}

func TestRoutineSatisfiesTheGeneralHouseholdSecretRuleNotAnExemption(t *testing.T) {
	db, vault, value := secretFixture(t)
	s, err := vault.Create("mika", SecretInput{Scope: "household", Label: "Calendar", Value: value})
	if err != nil {
		t.Fatal(err)
	}
	if err = vault.AgentGrant("mika", s.ID, "shared", "calendar.get_agenda", true); err != nil {
		t.Fatal(err)
	}
	_, routine := approvedResourceRoutine(t, db, RoutineResources{SecretIDs: []string{s.ID}}, AudienceHousehold)
	if err = vault.UseRoutine(s.ID, routine, "calendar.get_agenda", func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err = vault.AgentGrant("mika", s.ID, "shared", "calendar.get_agenda", false); err != nil {
		t.Fatal(err)
	}
	if err = vault.UseRoutine(s.ID, routine, "calendar.get_agenda", func([]byte) error { t.Fatal("revoked Agent capability used"); return nil }); err == nil {
		t.Fatal("routine grant replaced Agent capability")
	}
	if err = vault.AgentGrant("mika", s.ID, "shared", "calendar.get_agenda", true); err != nil {
		t.Fatal(err)
	}
	if _, err = db.sql.Exec("DELETE FROM household_secret_routine_grants WHERE plan=? AND secret_id=?", routine.plan, s.ID); err != nil {
		t.Fatal(err)
	}
	if err = vault.UseRoutine(s.ID, routine, "calendar.get_agenda", func([]byte) error { t.Fatal("approved routine bypassed resource grant"); return nil }); err == nil {
		t.Fatal("approval substituted for the general principal/object grant")
	}
}

func TestRoutineSecretGrantIsExactVersionAndAgentCapabilityStillRequired(t *testing.T) {
	db, vault, value := secretFixture(t)
	s, err := vault.Create("mika", SecretInput{Scope: "household", Label: "Calendar", Value: value})
	if err != nil {
		t.Fatal(err)
	}
	if err = vault.AgentGrant("mika", s.ID, "shared", "calendar.get_agenda", true); err != nil {
		t.Fatal(err)
	}
	_, old := approvedResourceRoutine(t, db, RoutineResources{SecretIDs: []string{s.ID}}, AudienceHousehold)
	called := false
	consume := func(raw []byte) error {
		called = true
		if string(raw) != value {
			t.Fatal("wrong value")
		}
		return nil
	}
	if err = vault.UseRoutine(s.ID, old, "calendar.get_agenda", consume); err != nil || !called {
		t.Fatalf("reviewed version denied: %v", err)
	}
	called = false
	if _, err = db.SubmitRoutinePlan("resource-run", "sam", "", AudienceHousehold, "household notification", "fixture", RoutineResources{}); err != nil {
		t.Fatal(err)
	}
	if err = vault.UseRoutine(s.ID, old, "calendar.get_agenda", consume); err == nil || called {
		t.Fatal("old approved version ran after grant edit")
	}
	r, err := db.GetHouseholdRoutine("resource-run")
	if err != nil || r.Active() {
		t.Fatal("changed grants did not require reapproval")
	}
	next, err := db.SubmitRoutinePlan("resource-run", "sam", "", AudienceHousehold, "household notification", "fixture", RoutineResources{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ApproveRoutinePlan(next, "mika"); err != nil {
		t.Fatal(err)
	}
	newPrincipal, err := db.BindRoutinePrincipal(next)
	if err != nil {
		t.Fatal(err)
	}
	if err = vault.UseRoutine(s.ID, newPrincipal, "calendar.get_agenda", consume); err == nil || called {
		t.Fatal("old version's grant carried into newly approved version")
	}
}

func TestAdultsOnlyRecordIsExcludedFromAKidsAudienceRoutine(t *testing.T) {
	db, _, _ := secretFixture(t)
	owner, err := db.GetButlerMember("mika")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddPersonToGroup(groupID(t, db, SeedAdults), owner.PersonID, "mika"); err != nil {
		t.Fatal(err)
	}
	adults := adultsAudience(t, db)
	child, err := db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AddPersonToGroup(groupID(t, db, SeedKids), child.PersonID, "mika"); err != nil {
		t.Fatal(err)
	}
	kids, err := db.CreateAudienceForGroups("Kids", groupID(t, db, SeedKids))
	if err != nil {
		t.Fatal(err)
	}
	m := fact("adult budget", HouseholdOwner, ButlerTaskAudience(adults))
	m.Scope = MemoryScopeHousehold
	seq, err := db.RecordMemory(m)
	if err != nil {
		t.Fatal(err)
	}
	_, principal := approvedResourceRoutine(t, db, RoutineResources{MemoryIDs: []int64{seq}}, kids.ID)
	hits, err := db.RetrieveRoutineMemory(principal, MemoryDestination{BuiltIn: true}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatal("adult record entered a routine delivering to a child")
	}
}

func TestRoutineMemoryNeedsExactGrantAndInheritedAudience(t *testing.T) {
	db, _, _ := secretFixture(t)
	household := scopedFact(t, MemoryScopeHousehold, "shared", HouseholdOwner)
	household.Content = "grocery list"
	id, err := db.RecordMemory(household)
	if err != nil {
		t.Fatal(err)
	}
	local := scopedFact(t, MemoryScopeAgentLocal, "shared", HouseholdOwner)
	local.Content = "local note"
	localID, err := db.RecordMemory(local)
	if err != nil {
		t.Fatal(err)
	}
	newButler(t, db, "other", "kitchen-box")
	otherLocal := scopedFact(t, MemoryScopeAgentLocal, "other", HouseholdOwner)
	otherLocal.Content = "another Agent's note"
	otherID, err := db.RecordMemory(otherLocal)
	if err != nil {
		t.Fatal(err)
	}
	ungranted := household
	ungranted.Content = "unreviewed record"
	ungrantedID, err := db.RecordMemory(ungranted)
	if err != nil {
		t.Fatal(err)
	}
	_, principal := approvedResourceRoutine(t, db, RoutineResources{MemoryIDs: []int64{id, localID, otherID}}, AudienceHousehold)
	d := MemoryDestination{BuiltIn: true}
	if err = db.CheckRoutineMemory(principal, ungrantedID, d); err == nil {
		t.Fatal("routine reached an ungranted record")
	}
	if err = db.CheckRoutineMemory(principal, otherID, d); err == nil {
		t.Fatal("routine reached another Agent's local record")
	}
	hits, err := db.RetrieveRoutineMemory(principal, d, 10)
	if err != nil || len(hits) != 2 {
		t.Fatalf("authorized Household and Agent-local records: %v %v", hits, err)
	}
	if err = db.CheckRoutineMemory(principal, id, d); err != nil {
		t.Fatal(err)
	}
	if err = db.SetAgentMemoryRestriction("shared", MemoryScopeHousehold, true, "mika"); err != nil {
		t.Fatal(err)
	}
	hits, err = db.RetrieveRoutineMemory(principal, d, 10)
	if err != nil || len(hits) != 1 || hits[0].Seq != localID {
		t.Fatal("Agent restriction did not narrow live retrieval", hits, err)
	}
	owner, _ := db.GetButlerMember("mika")
	if err = db.AddPersonToGroup(groupID(t, db, SeedAdults), owner.PersonID, "mika"); err != nil {
		t.Fatal(err)
	}
	adults := adultsAudience(t, db)
	if _, err = db.sql.Exec("INSERT INTO memory_constraints(memory,audience) VALUES(?,?)", localID, adults); err != nil {
		t.Fatal(err)
	}
	hits, err = db.RetrieveRoutineMemory(principal, d, 10)
	if err != nil || len(hits) != 0 {
		t.Fatal("Agent-local record escaped inherited source restriction", hits, err)
	}
}

func TestRoutineCannotReachPersonalMemoryBecauseAnOwnerApprovedIt(t *testing.T) {
	db, _, _ := secretFixture(t)
	m := scopedFact(t, MemoryScopePersonal, "shared", "sam")
	id, err := db.RecordMemory(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.CreateHouseholdRoutine(HouseholdRoutine{Name: "resource-run", Ownership: RoutineWildstone, Author: "sam", Document: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err = db.AssignRoutineButler("resource-run", "shared", "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.SubmitRoutinePlan("resource-run", "sam", "", AudienceHousehold, "household notification", "fixture", RoutineResources{MemoryIDs: []int64{id}}); err == nil {
		t.Fatal("Owner review could grant a Person's Personal memory")
	}
}

func TestRoutineMemoryAdmitsOnlyALiveCompatibleDirectDestination(t *testing.T) {
	db, _, _ := secretFixture(t)
	at := connect(t, db, "shared", "telegram", ButlerSurfaceFamily)
	bind(t, db, at, "sam", "sam-chat")
	if err := db.GrantButlerAccess("shared", "sam", "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE connection_disclosure SET retention='provider' WHERE connection=?`, at.Seq); err != nil {
		t.Fatal(err)
	}
	sam, err := db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	_, principal := approvedResourceRoutine(t, db, RoutineResources{}, AudienceHousehold)
	destination := MemoryDestination{ConnectionRef: at.Seq, RecipientPerson: sam.PersonID}
	if hits, err := db.RetrieveRoutineMemory(principal, destination, 10); err != nil || len(hits) != 0 {
		t.Fatalf("valid direct destination blocked before retrieval: %v %v", hits, err)
	}
	other := connect(t, db, "other", "telegram", ButlerSurfaceFamily, "other-chat")
	bind(t, db, other, "sam", "sam-other-chat")
	if err := db.GrantButlerAccess("other", "sam", "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE connection_disclosure SET retention='provider' WHERE connection=?`, other.Seq); err != nil {
		t.Fatal(err)
	}
	if hits, err := db.RetrieveRoutineMemory(principal, MemoryDestination{ConnectionRef: other.Seq, RecipientPerson: sam.PersonID}, 10); err != nil || len(hits) != 0 {
		t.Fatalf("valid delivery Agent without memory grants blocked inference: %v %v", hits, err)
	}
	for name, bad := range map[string]MemoryDestination{
		"unknown":           {},
		"missing recipient": {ConnectionRef: at.Seq},
		"wrong recipient":   {ConnectionRef: at.Seq, RecipientPerson: "mika"},
		"group":             {ConnectionRef: at.Seq, RecipientPerson: sam.PersonID, Group: true},
	} {
		if _, err := db.RetrieveRoutineMemory(principal, bad, 10); err == nil {
			t.Fatalf("%s destination admitted", name)
		}
	}
	if _, err := db.sql.Exec(`UPDATE connection_disclosure SET retention='unknown' WHERE connection=?`, at.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RetrieveRoutineMemory(principal, destination, 10); err == nil {
		t.Fatal("unknown disclosure admitted")
	}
	if _, err := db.sql.Exec(`UPDATE connection_disclosure SET retention='provider' WHERE connection=?`, at.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE chat_bindings SET revoked_at='revoked' WHERE connection=?`, at.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RetrieveRoutineMemory(principal, destination, 10); err == nil {
		t.Fatal("revoked direct binding admitted")
	}
}

func TestRoutineMemoryDisclosesOnlyCompatibleGrantedChatOrigins(t *testing.T) {
	db, _, _ := secretFixture(t)
	at := connect(t, db, "shared", "telegram", ButlerSurfaceFamily)
	bind(t, db, at, "sam", "sam-chat")
	if err := db.GrantButlerAccess("shared", "sam", "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE connection_disclosure SET retention='provider' WHERE connection=?`, at.Seq); err != nil {
		t.Fatal(err)
	}
	task, err := db.InsertButlerTask(ButlerTask{Kind: ButlerTaskConversation, Author: "sam", Owner: "sam", Surface: ButlerSurfaceFamily,
		AgentClass: AgentClassMember, Audience: ButlerAudiencePrivate, Objective: "remember", OriginConnection: at.Name(), OriginConnectionRef: at.Seq})
	if err != nil {
		t.Fatal(err)
	}
	memory := scopedFact(t, MemoryScopeHousehold, "shared", HouseholdOwner)
	memory.Content, memory.SourceKind, memory.SourceID = "shared grocery reminder", "conversation", task
	id, err := db.RecordMemory(memory)
	if err != nil {
		t.Fatal(err)
	}
	_, principal := approvedResourceRoutine(t, db, RoutineResources{MemoryIDs: []int64{id}}, AudienceHousehold)
	sam, err := db.GetButlerMember("sam")
	if err != nil {
		t.Fatal(err)
	}
	destination := MemoryDestination{ConnectionRef: at.Seq, RecipientPerson: sam.PersonID}
	if hits, err := db.RetrieveRoutineMemory(principal, destination, 10); err != nil || len(hits) != 1 || hits[0].Seq != id {
		t.Fatalf("compatible granted memory unavailable: %v %v", hits, err)
	}
	if _, err := db.sql.Exec(`UPDATE connection_disclosure SET encryption='end_to_end' WHERE connection=?`, at.Seq); err != nil {
		t.Fatal(err)
	}
	if hits, err := db.RetrieveRoutineMemory(principal, destination, 10); err != nil || len(hits) != 0 {
		t.Fatalf("changed disclosure leaked memory: %v %v", hits, err)
	}
}
