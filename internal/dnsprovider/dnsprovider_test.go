/*
Copyright 2023 Huaweicloud

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package dnsprovider

import (
	"context"
	"errors"
	"testing"

	dnsMdl "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/dns/v2/model"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
)

func TestPartitionUpdates(t *testing.T) {
	ep := func(name, recordType string, targets ...string) *endpoint.Endpoint {
		return &endpoint.Endpoint{
			DNSName:    name,
			RecordType: recordType,
			Targets:    endpoint.Targets(targets),
		}
	}

	cases := []struct {
		name              string
		old, new          []*endpoint.Endpoint
		wantInPlace       int
		wantFallbackPairs int
	}{
		{
			name:              "same type → in-place",
			old:               []*endpoint.Endpoint{ep("a.example.com", "A", "1.2.3.4")},
			new:               []*endpoint.Endpoint{ep("a.example.com", "A", "5.6.7.8")},
			wantInPlace:       1,
			wantFallbackPairs: 0,
		},
		{
			name:              "type change → fallback",
			old:               []*endpoint.Endpoint{ep("a.example.com", "A", "1.2.3.4")},
			new:               []*endpoint.Endpoint{ep("a.example.com", "AAAA", "::1")},
			wantInPlace:       0,
			wantFallbackPairs: 1,
		},
		{
			name: "mixed batch",
			old: []*endpoint.Endpoint{
				ep("a.example.com", "A", "1.2.3.4"),
				ep("b.example.com", "A", "1.2.3.4"),
			},
			new: []*endpoint.Endpoint{
				ep("a.example.com", "A", "5.6.7.8"),
				ep("b.example.com", "AAAA", "::1"),
			},
			wantInPlace:       1,
			wantFallbackPairs: 1,
		},
		{
			name:              "name mismatch → fallback (defensive)",
			old:               []*endpoint.Endpoint{ep("a.example.com", "A", "1.2.3.4")},
			new:               []*endpoint.Endpoint{ep("b.example.com", "A", "5.6.7.8")},
			wantInPlace:       0,
			wantFallbackPairs: 1,
		},
		{
			name:              "length mismatch → all fallback",
			old:               []*endpoint.Endpoint{ep("a.example.com", "A", "1.2.3.4")},
			new:               []*endpoint.Endpoint{},
			wantInPlace:       0,
			wantFallbackPairs: 0, // fallbackOld has 1, fallbackNew has 0; pair count is 0
		},
		{
			name:              "empty input",
			old:               nil,
			new:               nil,
			wantInPlace:       0,
			wantFallbackPairs: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inPlace, fbOld, fbNew := partitionUpdates(tc.old, tc.new)
			if got := len(inPlace); got != tc.wantInPlace {
				t.Errorf("inPlace count = %d, want %d", got, tc.wantInPlace)
			}
			// On length mismatches we deliberately emit unequal fallback slices,
			// so test only the equal-length cases against wantFallbackPairs.
			if len(tc.old) == len(tc.new) {
				if len(fbOld) != len(fbNew) {
					t.Errorf("fallbackOld/fallbackNew length disagree: %d vs %d", len(fbOld), len(fbNew))
				}
				if got := len(fbOld); got != tc.wantFallbackPairs {
					t.Errorf("fallback pair count = %d, want %d", got, tc.wantFallbackPairs)
				}
			}
		})
	}
}

func TestApplyChanges_InPlaceUpdateBypassesDeleteAndCreate(t *testing.T) {
	fake := newFakeDNSClient()
	fake.seedZone("zone-1", "example.com.")
	fake.seedRecordset("zone-1", "rs-1", "a.example.com.", "A", []string{"1.2.3.4"}, 60)

	p := newTestProvider(fake)

	changes := &plan.Changes{
		UpdateOld: []*endpoint.Endpoint{{
			DNSName:    "a.example.com",
			RecordType: "A",
			Targets:    endpoint.Targets{"1.2.3.4"},
			RecordTTL:  60,
		}},
		UpdateNew: []*endpoint.Endpoint{{
			DNSName:    "a.example.com",
			RecordType: "A",
			Targets:    endpoint.Targets{"5.6.7.8"},
			RecordTTL:  300,
		}},
	}

	if err := p.ApplyChanges(context.Background(), changes); err != nil {
		t.Fatalf("ApplyChanges returned error: %v", err)
	}

	if len(fake.deleteCalls) != 0 {
		t.Errorf("expected zero DeleteRecordSet calls, got %d (delete permission must not be required for in-place updates)", len(fake.deleteCalls))
	}
	if len(fake.createCalls) != 0 {
		t.Errorf("expected zero CreateRecordSet calls, got %d", len(fake.createCalls))
	}
	if len(fake.updateCalls) != 1 {
		t.Fatalf("expected exactly one UpdateRecordSets call, got %d", len(fake.updateCalls))
	}
	got := fake.updateCalls[0]
	if got.RecordsetId != "rs-1" {
		t.Errorf("UpdateRecordSets called with RecordsetId=%q, want %q", got.RecordsetId, "rs-1")
	}
	if got.Body == nil || got.Body.Records == nil || (*got.Body.Records)[0] != "5.6.7.8" {
		t.Errorf("UpdateRecordSets did not carry the new target; got body=%+v", got.Body)
	}
	if got.Body.Ttl == nil || *got.Body.Ttl != 300 {
		t.Errorf("UpdateRecordSets did not carry the new TTL; got %+v", got.Body.Ttl)
	}
}

func TestApplyChanges_TypeChangeFallsBackToDeleteCreate(t *testing.T) {
	fake := newFakeDNSClient()
	fake.seedZone("zone-1", "example.com.")
	fake.seedRecordset("zone-1", "rs-1", "a.example.com.", "A", []string{"1.2.3.4"}, 60)

	p := newTestProvider(fake)

	changes := &plan.Changes{
		UpdateOld: []*endpoint.Endpoint{{
			DNSName:    "a.example.com",
			RecordType: "A",
			Targets:    endpoint.Targets{"1.2.3.4"},
		}},
		UpdateNew: []*endpoint.Endpoint{{
			DNSName:    "a.example.com",
			RecordType: "AAAA",
			Targets:    endpoint.Targets{"::1"},
		}},
	}

	if err := p.ApplyChanges(context.Background(), changes); err != nil {
		t.Fatalf("ApplyChanges returned error: %v", err)
	}

	if len(fake.updateCalls) != 0 {
		t.Errorf("expected zero UpdateRecordSets calls on type change, got %d", len(fake.updateCalls))
	}
	if len(fake.deleteCalls) != 1 {
		t.Errorf("expected exactly one DeleteRecordSet call on type change, got %d", len(fake.deleteCalls))
	}
	if len(fake.createCalls) != 1 {
		t.Errorf("expected exactly one CreateRecordSet call on type change, got %d", len(fake.createCalls))
	}
}

func TestApplyChanges_UpdateFailureSurfacesAsSoftError(t *testing.T) {
	fake := newFakeDNSClient()
	fake.seedZone("zone-1", "example.com.")
	fake.seedRecordset("zone-1", "rs-1", "a.example.com.", "A", []string{"1.2.3.4"}, 60)
	fake.updateErr = errors.New("simulated huawei outage")

	p := newTestProvider(fake)

	changes := &plan.Changes{
		UpdateOld: []*endpoint.Endpoint{{
			DNSName: "a.example.com", RecordType: "A", Targets: endpoint.Targets{"1.2.3.4"},
		}},
		UpdateNew: []*endpoint.Endpoint{{
			DNSName: "a.example.com", RecordType: "A", Targets: endpoint.Targets{"5.6.7.8"},
		}},
	}

	err := p.ApplyChanges(context.Background(), changes)
	if err == nil {
		t.Fatal("expected ApplyChanges to surface update failure, got nil")
	}
	// Critical invariant: a failing in-place update must NOT silently fall back
	// to delete+create. That would re-introduce the very gap this fix removes.
	if len(fake.deleteCalls) != 0 {
		t.Errorf("update failure must not trigger a delete fallback; got %d delete calls", len(fake.deleteCalls))
	}
	if len(fake.createCalls) != 0 {
		t.Errorf("update failure must not trigger a create fallback; got %d create calls", len(fake.createCalls))
	}
}

// --- test helpers -----------------------------------------------------------

func newTestProvider(client HuaweiCloudDNSAPI) *HuaweicloudProvider {
	return &HuaweicloudProvider{
		domainFilter: endpoint.NewDomainFilter(nil),
		zoneIDFilter: provider.NewZoneIDFilter(nil),
		dnsClient:    client,
	}
}

type fakeDNSClient struct {
	zones      []dnsMdl.PrivateZoneResp
	recordsets map[string][]dnsMdl.ListRecordSets

	createCalls []*dnsMdl.CreateRecordSetRequest
	deleteCalls []*dnsMdl.DeleteRecordSetRequest
	updateCalls []*dnsMdl.UpdateRecordSetsRequest

	updateErr error
}

func newFakeDNSClient() *fakeDNSClient {
	return &fakeDNSClient{recordsets: map[string][]dnsMdl.ListRecordSets{}}
}

func (f *fakeDNSClient) seedZone(id, name string) {
	idCopy, nameCopy := id, name
	f.zones = append(f.zones, dnsMdl.PrivateZoneResp{Id: &idCopy, Name: &nameCopy})
}

func (f *fakeDNSClient) seedRecordset(zoneID, rsID, name, recordType string, targets []string, ttl int32) {
	rsIDCopy, nameCopy, typeCopy := rsID, name, recordType
	targetsCopy := append([]string(nil), targets...)
	ttlCopy := ttl
	defaultFlag := false
	f.recordsets[zoneID] = append(f.recordsets[zoneID], dnsMdl.ListRecordSets{
		Id:      &rsIDCopy,
		Name:    &nameCopy,
		Type:    &typeCopy,
		Records: &targetsCopy,
		Ttl:     &ttlCopy,
		Default: &defaultFlag,
	})
}

func (f *fakeDNSClient) ListPrivateZones(req *dnsMdl.ListPrivateZonesRequest) (*dnsMdl.ListPrivateZonesResponse, error) {
	zones := f.zones
	total := int32(len(zones))
	return &dnsMdl.ListPrivateZonesResponse{
		Zones:    &zones,
		Metadata: &dnsMdl.Metadata{TotalCount: &total},
	}, nil
}

func (f *fakeDNSClient) ListRecordSetsByZone(req *dnsMdl.ListRecordSetsByZoneRequest) (*dnsMdl.ListRecordSetsByZoneResponse, error) {
	sets := f.recordsets[req.ZoneId]
	total := int32(len(sets))
	return &dnsMdl.ListRecordSetsByZoneResponse{
		Recordsets: &sets,
		Metadata:   &dnsMdl.Metadata{TotalCount: &total},
	}, nil
}

func (f *fakeDNSClient) ShowPrivateZone(req *dnsMdl.ShowPrivateZoneRequest) (*dnsMdl.ShowPrivateZoneResponse, error) {
	return &dnsMdl.ShowPrivateZoneResponse{Routers: &[]dnsMdl.Router{}}, nil
}

func (f *fakeDNSClient) CreateRecordSet(req *dnsMdl.CreateRecordSetRequest) (*dnsMdl.CreateRecordSetResponse, error) {
	f.createCalls = append(f.createCalls, req)
	id := "new-rs"
	return &dnsMdl.CreateRecordSetResponse{Id: &id}, nil
}

func (f *fakeDNSClient) DeleteRecordSet(req *dnsMdl.DeleteRecordSetRequest) (*dnsMdl.DeleteRecordSetResponse, error) {
	f.deleteCalls = append(f.deleteCalls, req)
	rs := req.RecordsetId
	return &dnsMdl.DeleteRecordSetResponse{Id: &rs}, nil
}

func (f *fakeDNSClient) UpdateRecordSets(req *dnsMdl.UpdateRecordSetsRequest) (*dnsMdl.UpdateRecordSetsResponse, error) {
	f.updateCalls = append(f.updateCalls, req)
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	rs := req.RecordsetId
	return &dnsMdl.UpdateRecordSetsResponse{Id: &rs}, nil
}
