package application

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	xrayfake "xpanel/internal/adapter/xray/fake"
	"xpanel/internal/domain"
	"xpanel/internal/persistence/sqlite"
	"xpanel/internal/ports"
	"xpanel/internal/security"
)

type featureFixture struct {
	store     *sqlite.Store
	keyring   *security.Keyring
	clock     *ports.FixedClock
	adapter   *xrayfake.Adapter
	templates *TemplateService
}

func appID(t *testing.T) domain.ID {
	t.Helper()
	id, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newFeatureFixture(t *testing.T) *featureFixture {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "xpanel.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db.Write); err != nil {
		t.Fatal(err)
	}
	store := sqlite.NewStore(db)
	keyring, err := security.NewKeyring(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	clock := &ports.FixedClock{Time: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	verifier, nonce, err := keyring.NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureSingletons(ctx, "UTC", "127.0.0.1:10085", "v26.3.27", verifier, nonce, clock.Now()); err != nil {
		t.Fatal(err)
	}
	adapter := xrayfake.New()
	adapter.Now = clock.Now
	target := ports.InstanceTarget{APIEndpoint: "127.0.0.1:10085", ExpectedVersion: "v26.3.27", RPCTimeout: time.Second}
	return &featureFixture{store: store, keyring: keyring, clock: clock, adapter: adapter,
		templates: NewTemplateService(store, adapter, keyring, clock, target, nil)}
}

func validTemplateInput(t *testing.T) TemplateInput {
	t.Helper()
	return TemplateInput{Name: "Primary", PublicHost: "vpn.example.com", ListenAddress: "127.0.0.1",
		PortPoolStart: 30000, PortPoolEnd: 30099, Method: security.MethodAES256, Network: domain.NetworkTCPUDP,
		RequestID: appID(t), ActorID: appID(t)}
}

func registerCompatibleTemplate(t *testing.T, fixture *featureFixture) domain.ID {
	t.Helper()
	id, err := fixture.templates.RegisterTemplate(context.Background(), validTemplateInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.templates.RunValidation(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	record, err := fixture.store.Template(context.Background(), id)
	if err != nil || record.Template.Compatibility != domain.CompatibilityCompatible {
		t.Fatalf("template = %#v, %v", record, err)
	}
	return id
}

// leaseForTest 把操作直接置为 "test" 持有的租约，供不经 worker 的确认测试使用（ConfirmSync 要求租约 owner 匹配）。
func leaseForTest(t *testing.T, fixture *featureFixture, id domain.ID) {
	t.Helper()
	if _, err := fixture.store.DB().Write.Exec(`UPDATE synchronization_operations SET state='leased',lease_owner='test',lease_expires_at=? WHERE id=?`,
		fixture.clock.Now().Add(time.Minute).UnixMilli(), id.String()); err != nil {
		t.Fatal(err)
	}
}

// 入站模板的登记、能力验证与端口池。
func TestTemplateRegistrationAndValidation(t *testing.T) {
	fixture := newFeatureFixture(t)
	id := registerCompatibleTemplate(t, fixture)
	record, err := fixture.store.Template(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if record.Template.Pool.Capacity() != 100 || record.Template.ListenAddress != "127.0.0.1" {
		t.Fatalf("template = %#v", record.Template)
	}
	usage, err := fixture.templates.PortUsage(context.Background(), id)
	if err != nil || usage.Remaining != 100 {
		t.Fatalf("usage = %#v, %v", usage, err)
	}
	// 节点不兼容时模板被标记为 incompatible 并附可理解原因。
	fixture.adapter.Templates[id.String()] = ports.TemplateCapabilities{InboundCreatable: true, ProtocolSupported: true,
		MethodSupported: true, MultiUserSupported: false, CompatibilityReason: "node does not support multi-user"}
	if _, err := fixture.templates.Revalidate(context.Background(), RevalidateInput{ID: id,
		ExpectedRevision: record.Template.Revision, RequestID: appID(t), ActorID: appID(t)}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.templates.RunValidation(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	after, _ := fixture.store.Template(context.Background(), id)
	if after.Template.Compatibility != domain.CompatibilityIncompatible || after.Template.CompatibilityReason == "" {
		t.Fatalf("incompatible template = %#v", after.Template)
	}
}
