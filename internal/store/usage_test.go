package store

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestEventsPayloadNamesEachOrganisationOnce(t *testing.T) {
	payload, ok := eventsPayload([]Event{
		{OrgID: "org_a"}, {OrgID: "org_b"}, {OrgID: "org_a"}, {OrgID: "org_c", BillsOnly: true},
	})
	if !ok {
		t.Fatal("a batch of requests announced nothing")
	}
	orgs, all := EventOrgs(payload)
	if all || !slices.Equal(orgs, []string{"org_a", "org_b"}) {
		t.Errorf("orgs = %v (all %v), want org_a and org_b", orgs, all)
	}
}

func TestEventsPayloadWithoutRequestsAnnouncesNothing(t *testing.T) {
	// Billing lines alone are not in the request log, so no stream has
	// anything to look for.
	if _, ok := eventsPayload([]Event{{OrgID: "org_a", BillsOnly: true}}); ok {
		t.Error("a batch without requests was announced")
	}
}

func TestEventsPayloadTooLongWakesEveryStream(t *testing.T) {
	var events []Event
	for i := range 500 {
		events = append(events, Event{OrgID: fmt.Sprintf("org_%03d_%s", i, strings.Repeat("x", 20))})
	}
	payload, ok := eventsPayload(events)
	if !ok || payload != "" {
		t.Fatalf("payload of %d bytes, want an empty one", len(payload))
	}
	if _, all := EventOrgs(payload); !all {
		t.Error("an empty payload did not wake every stream")
	}
}
