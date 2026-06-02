package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pulumi/pulumi-cloudflare/sdk/v5/go/cloudflare"
	"github.com/pulumi/pulumi-command/sdk/go/command/remote"
	"github.com/pulumi/pulumi-hcloud/sdk/go/hcloud"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// Defaults for stack config. Values without per-environment
// variation (network shape, internal IPs, base image) stay as
// consts. Values that might differ between environments (zone,
// region, server size) become Pulumi config keys with these
// defaults.
const (
	defaultZoneName    = "snoringcat.games"
	defaultLocation    = "hil"
	defaultNetworkZone = "us-west"
	// CPX11 (2 vCPU / 2 GB / 40 GB) is the cheapest non-EU
	// Hetzner tier in Hillsboro. CAX (ARM) isn't available in
	// Hillsboro, and CX (Intel) is EU-only too. Hillsboro
	// offers CPX (AMD shared) and CCX (AMD dedicated).
	//
	// Cost: ~€6.99/mo cap. The 2026-05-06 consolidation
	// collapsed the original 2x CPX11 (Nakama + Postgres
	// separate hosts, full Prometheus/Grafana/Loki/Promtail
	// observability) into a single CPX11 with Postgres co-
	// tenanted alongside Nakama and the obs stack stripped to
	// fit on 2 GB RAM. 1x CPX21 = 1x CPX11+CPX11 in this
	// region, so the only path to actual savings was single-
	// CPX11 + stripped stack.
	//
	// Stage 7.11 (2026-05-13) re-introduced a lightweight obs
	// subset (Prometheus + Grafana + node-exporter +
	// postgres-exporter) onto the same box. Loki + Promtail
	// stayed off — logs continue via `journalctl` /
	// `docker logs`. Headroom check before re-enabling:
	// 603 MB used + 1.3 GB available on the live box; the
	// added services land in ~350 MB resident, leaving
	// comfortable margin.
	//
	// Stepping back UP (cpx11→cpx21) is allowed in-place at
	// any time if observed usage demands it. Stepping
	// DOWN (cpx21→cpx11) is forbidden by Hetzner (smaller
	// disk; disk-shrink isn't allowed) and would require a
	// destroy+recreate with Postgres dump/restore.
	defaultServerType = "cpx11"
	defaultImage      = "ubuntu-24.04"

	// Internal network shape — stays in code because changing
	// these would require a state migration, not just a config
	// override. The private network is now used only for
	// future-host expansion; in the single-host stack the
	// nakama box doesn't currently talk to any other private
	// peer.
	networkIPRange  = "10.0.0.0/16"
	subnetIPRange   = "10.0.1.0/24"
	nakamaPrivateIP = "10.0.1.10"
)

// stackConfig collects the per-stack overrides from
// Pulumi.<stack>.yaml. All values fall back to the `default*`
// constants above when the corresponding key is unset.
type stackConfig struct {
	ZoneName    string
	Location    string
	NetworkZone string
	ServerType  string
	Image       string
	AdminCidr   string
}

func loadStackConfig(cfg *config.Config) stackConfig {
	pickWithDefault := func(key, def string) string {
		v := cfg.Get(key)
		if v == "" {
			return def
		}
		return v
	}
	return stackConfig{
		ZoneName:    pickWithDefault("zoneName", defaultZoneName),
		Location:    pickWithDefault("location", defaultLocation),
		NetworkZone: pickWithDefault("networkZone", defaultNetworkZone),
		ServerType:  pickWithDefault("serverType", defaultServerType),
		Image:       pickWithDefault("image", defaultImage),
		AdminCidr:   cfg.Get("adminCidr"),
	}
}

// idToInt converts a Pulumi IDOutput (string) to IntOutput, which is
// required by hcloud SDK fields like NetworkSubnet.NetworkId and
// FirewallApplyTo.Server.
func idToInt(o pulumi.IDOutput) pulumi.IntOutput {
	return o.ApplyT(func(id pulumi.ID) (int, error) {
		n, err := strconv.Atoi(string(id))
		if err != nil {
			return 0, fmt.Errorf("convert id %q to int: %w", id, err)
		}
		return n, nil
	}).(pulumi.IntOutput)
}

const baseCloudInit = `#cloud-config
package_update: true
package_upgrade: true
packages:
  - apt-transport-https
  - ca-certificates
  - curl
  - gnupg
  - lsb-release
  - fail2ban
  - ufw
  - jq
write_files:
  - path: /etc/fail2ban/jail.local
    content: |
      [sshd]
      enabled = true
      bantime = 1h
      findtime = 10m
      maxretry = 5
runcmd:
  - curl -fsSL https://get.docker.com -o /tmp/get-docker.sh
  - sh /tmp/get-docker.sh
  - systemctl enable --now docker
  - ufw default deny incoming
  - ufw default allow outgoing
  - ufw allow ssh
  # Private network is the trust boundary between platform hosts
  # (Nakama ↔ Postgres on 10.0.1.0/24 + Prometheus scrapes of
  # node-exporter / postgres-exporter). Without this, the
  # cross-host scrapes fail and Postgres connections from Nakama
  # are blocked. Subnet matches networkIPRange / subnetIPRange
  # constants above.
  - ufw allow from 10.0.0.0/16 to any
  - systemctl enable --now fail2ban
`

const nakamaExtraRuncmd = `  - ufw allow 80/tcp
  - ufw allow 443/tcp
  - ufw --force enable
  - touch /var/lib/cloud/snoringcat-bootstrap-done
`

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		cfg := config.New(ctx, "")
		sc := loadStackConfig(cfg)

		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("user home dir: %w", err)
		}
		nakamaPubPath := cfg.Get("nakamaSshPubkeyPath")
		if nakamaPubPath == "" {
			nakamaPubPath = filepath.Join(home, ".hopnbop-migration", "ssh", "nakama.pub")
		}
		// Private key for nakama root: required to provision the
		// non-root `watchdog` user via remote.Command. See the
		// nakamaWatchdogUser resource below.
		nakamaPrivPath := cfg.Get("nakamaSshPrivkeyPath")
		if nakamaPrivPath == "" {
			nakamaPrivPath = filepath.Join(home, ".hopnbop-migration", "ssh", "nakama")
		}
		// Public key for the `watchdog` SSH user (separate from the
		// hcloud root key; deployed by remote.Command rather than
		// cloud-init).
		watchdogPubPath := cfg.Get("watchdogSshPubkeyPath")
		if watchdogPubPath == "" {
			watchdogPubPath = filepath.Join(home, ".hopnbop-migration", "ssh", "nakama-watchdog.pub")
		}

		nakamaPub, err := os.ReadFile(nakamaPubPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", nakamaPubPath, err)
		}
		nakamaPriv, err := os.ReadFile(nakamaPrivPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", nakamaPrivPath, err)
		}
		watchdogPub, err := os.ReadFile(watchdogPubPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", watchdogPubPath, err)
		}

		zone, err := cloudflare.LookupZone(ctx, &cloudflare.LookupZoneArgs{
			Name: pulumi.StringRef(sc.ZoneName),
		})
		if err != nil {
			return fmt.Errorf("lookup cloudflare zone %s: %w", sc.ZoneName, err)
		}

		nakamaKey, err := hcloud.NewSshKey(ctx, "nakama", &hcloud.SshKeyArgs{
			Name:      pulumi.String("nakama"),
			PublicKey: pulumi.String(strings.TrimSpace(string(nakamaPub))),
		})
		if err != nil {
			return err
		}

		network, err := hcloud.NewNetwork(ctx, "snoringcat-internal", &hcloud.NetworkArgs{
			Name:    pulumi.String("snoringcat-internal"),
			IpRange: pulumi.String(networkIPRange),
		})
		if err != nil {
			return err
		}
		subnet, err := hcloud.NewNetworkSubnet(ctx, "snoringcat-subnet", &hcloud.NetworkSubnetArgs{
			NetworkId:   idToInt(network.ID()),
			Type:        pulumi.String("cloud"),
			NetworkZone: pulumi.String(sc.NetworkZone),
			IpRange:     pulumi.String(subnetIPRange),
		})
		if err != nil {
			return err
		}

		nakamaUserData := baseCloudInit + nakamaExtraRuncmd

		// userData is only consumed on first boot. Hetzner's API
		// returns a SHA1 hash on subsequent reads (not the
		// original plaintext), which Pulumi sees as drift and
		// would force replacement on every apply. Ignore it; if
		// we ever genuinely need to re-bootstrap a host we
		// rebuild it explicitly.
		nakamaSrv, err := hcloud.NewServer(ctx, "nakama-prod-1", &hcloud.ServerArgs{
			Name:       pulumi.String("nakama-prod-1"),
			ServerType: pulumi.String(sc.ServerType),
			Image:      pulumi.String(sc.Image),
			Location:   pulumi.String(sc.Location),
			SshKeys:    pulumi.StringArray{nakamaKey.Name},
			UserData:   pulumi.String(nakamaUserData),
			Networks: hcloud.ServerNetworkTypeArray{
				&hcloud.ServerNetworkTypeArgs{
					NetworkId: idToInt(network.ID()),
					Ip:        pulumi.String(nakamaPrivateIP),
				},
			},
		}, pulumi.DependsOn([]pulumi.Resource{subnet}),
			pulumi.IgnoreChanges([]string{"userData"}),
			// Hetzner server names are unique within the
			// account; create-before-delete on replacement fails
			// because the new server can't take a name still
			// owned by the old one. Force delete-first.
			pulumi.DeleteBeforeReplace(true))
		if err != nil {
			return err
		}

		// Provision a non-root `watchdog` user on nakama-prod-1 with a
		// forced-command authorized_keys for the daily job-watchdog.
		// The forced command limits the holder of the watchdog key to
		// `systemctl --failed` + `systemctl list-timers` (read-only
		// systemd state).
		//
		// Why a separate Pulumi resource rather than cloud-init:
		// nakamaSrv has IgnoreChanges([]string{"userData"}) so editing
		// the cloud-config in this file has no effect on the running
		// host. remote.Command runs imperatively against the deployed
		// host and re-fires when its inputs change (key rotation).
		// Idempotent: the script tolerates re-runs.
		watchdogAuthLine := strings.Join([]string{
			`command="systemctl --failed --no-pager --plain ` +
				`&& echo ===TIMERS=== ` +
				`&& systemctl list-timers --all --no-pager",` +
				`no-port-forwarding,no-X11-forwarding,` +
				`no-agent-forwarding,no-pty`,
			strings.TrimSpace(string(watchdogPub)),
		}, " ")
		watchdogProvisionScript := `set -e
# Wait for cloud-init to finish on a freshly-rebuilt host.
while [ ! -f /var/lib/cloud/snoringcat-bootstrap-done ]; do
    echo "waiting for cloud-init bootstrap..."
    sleep 10
done
id watchdog >/dev/null 2>&1 || \
    useradd --system --create-home --shell /bin/bash watchdog
# Unlock the account: useradd --system leaves it password-locked
# (! in shadow), and UsePAM yes rejects locked accounts even for
# pubkey auth. Using * means "no usable password" without locking.
usermod -p '*' watchdog
install -d -m 0700 -o watchdog -g watchdog /home/watchdog/.ssh
cat > /home/watchdog/.ssh/authorized_keys <<'KEY_EOF'
` + watchdogAuthLine + `
KEY_EOF
chown watchdog:watchdog /home/watchdog/.ssh/authorized_keys
chmod 600 /home/watchdog/.ssh/authorized_keys
echo "watchdog user provisioned"
`
		nakamaRootKeySecret := pulumi.ToSecret(
			pulumi.String(string(nakamaPriv)),
		).(pulumi.StringOutput)
		_, err = remote.NewCommand(ctx, "nakama-watchdog-user", &remote.CommandArgs{
			Connection: &remote.ConnectionArgs{
				Host:       nakamaSrv.Ipv4Address,
				User:       pulumi.String("root"),
				PrivateKey: nakamaRootKeySecret,
			},
			Create: pulumi.String(watchdogProvisionScript),
			Update: pulumi.String(watchdogProvisionScript),
			// Re-fire the script when the public key changes (e.g.
			// during key rotation) or when the script itself changes.
			Triggers: pulumi.Array{
				pulumi.String(watchdogAuthLine),
				pulumi.String(watchdogProvisionScript),
			},
		}, pulumi.DependsOn([]pulumi.Resource{nakamaSrv}))
		if err != nil {
			return err
		}

		// Firewalls (Hetzner Cloud Firewall, applied to each server).
		// adminSshSources controls who can SSH. Empty config means open
		// to the world (relies on key-based auth + fail2ban).
		adminSshSources := pulumi.StringArray{
			pulumi.String("0.0.0.0/0"),
			pulumi.String("::/0"),
		}
		if sc.AdminCidr != "" {
			adminSshSources = pulumi.StringArray{pulumi.String(sc.AdminCidr)}
		}
		worldIPv46 := pulumi.StringArray{
			pulumi.String("0.0.0.0/0"),
			pulumi.String("::/0"),
		}

		_, err = hcloud.NewFirewall(ctx, "nakama-fw", &hcloud.FirewallArgs{
			Name: pulumi.String("nakama-fw"),
			Rules: hcloud.FirewallRuleArray{
				&hcloud.FirewallRuleArgs{
					Direction: pulumi.String("in"),
					Protocol:  pulumi.String("tcp"),
					Port:      pulumi.String("22"),
					SourceIps: adminSshSources,
				},
				&hcloud.FirewallRuleArgs{
					Direction: pulumi.String("in"),
					Protocol:  pulumi.String("tcp"),
					Port:      pulumi.String("80"),
					SourceIps: worldIPv46,
				},
				&hcloud.FirewallRuleArgs{
					Direction: pulumi.String("in"),
					Protocol:  pulumi.String("tcp"),
					Port:      pulumi.String("443"),
					SourceIps: worldIPv46,
				},
				// Local Docker allocator port ranges. Game-server
				// containers spawned by the LocalDockerAllocator
				// bind to host ports in these ranges (defaults in
				// runtime/local_allocator.go) and need to be
				// reachable directly from clients. UDP for ENet
				// game traffic + WebRTC DataChannels; TCP for the
				// signaling WebSocket. Edgegap-only deployments
				// don't use these ranges (Edgegap servers are on
				// their own hosts entirely), so leaving the rule
				// in place when local mode is off has no security
				// impact — there's nothing listening.
				&hcloud.FirewallRuleArgs{
					Direction: pulumi.String("in"),
					Protocol:  pulumi.String("udp"),
					Port:      pulumi.String("30000-30099"),
					SourceIps: worldIPv46,
				},
				&hcloud.FirewallRuleArgs{
					Direction: pulumi.String("in"),
					Protocol:  pulumi.String("tcp"),
					Port:      pulumi.String("30100-30199"),
					SourceIps: worldIPv46,
				},
			},
			ApplyTos: hcloud.FirewallApplyToArray{
				&hcloud.FirewallApplyToArgs{
					Server: idToInt(nakamaSrv.ID()),
				},
			},
		})
		if err != nil {
			return err
		}

		// Cloudflare DNS A record. Proxy off (gray cloud) — Nakama uses
		// long-lived WebSocket / gRPC, doesn't need CDN caching.
		nakamaRecord, err := cloudflare.NewRecord(ctx, "nakama-a", &cloudflare.RecordArgs{
			ZoneId:  pulumi.String(zone.Id),
			Name:    pulumi.String("nakama"),
			Type:    pulumi.String("A"),
			Content: nakamaSrv.Ipv4Address,
			Proxied: pulumi.Bool(false),
			Ttl:     pulumi.Int(1),
			Comment: pulumi.String("Phase A: Nakama backend"),
		})
		if err != nil {
			return err
		}

		// Stable hostname in front of the WebSocket signaling
		// proxy (infra/remote/signaling-proxy/). Replaces the
		// per-deploy s-<dashed-ip>.game.hopnbop.net pattern,
		// which suffered DNS-propagation races with home / ISP
		// resolvers caching NXDOMAIN before the runtime hook's
		// pre-warm completed. Same A target as nakama-a (single-
		// host deployment); Caddy multiplexes by Host header.
		signalingRecord, err := cloudflare.NewRecord(ctx, "signaling-a", &cloudflare.RecordArgs{
			ZoneId:  pulumi.String(zone.Id),
			Name:    pulumi.String("signaling"),
			Type:    pulumi.String("A"),
			Content: nakamaSrv.Ipv4Address,
			Proxied: pulumi.Bool(false),
			Ttl:     pulumi.Int(1),
			Comment: pulumi.String("WebRTC/WS signaling proxy"),
		})
		if err != nil {
			return err
		}

		// Grafana UI. Restored 2026-05-13 in Stage 7.11 after
		// the 2026-05-06 consolidation tore it down. Same A
		// target as nakama-a / signaling-a (single-host
		// deployment); Caddy multiplexes by Host header.
		grafanaRecord, err := cloudflare.NewRecord(ctx, "grafana-a", &cloudflare.RecordArgs{
			ZoneId:  pulumi.String(zone.Id),
			Name:    pulumi.String("grafana"),
			Type:    pulumi.String("A"),
			Content: nakamaSrv.Ipv4Address,
			Proxied: pulumi.Bool(false),
			Ttl:     pulumi.Int(1),
			Comment: pulumi.String("Grafana (lightweight obs)"),
		})
		if err != nil {
			return err
		}

		ctx.Export("nakama_server_id", nakamaSrv.ID())
		ctx.Export("nakama_public_ip", nakamaSrv.Ipv4Address)
		ctx.Export("nakama_private_ip", pulumi.String(nakamaPrivateIP))
		ctx.Export("private_network_id", network.ID())
		ctx.Export("zone_id", pulumi.String(zone.Id))
		ctx.Export("nakama_dns_record_id", nakamaRecord.ID())
		ctx.Export("nakama_url", pulumi.String("https://nakama."+sc.ZoneName))
		ctx.Export("signaling_dns_record_id", signalingRecord.ID())
		ctx.Export("signaling_url", pulumi.String("https://signaling."+sc.ZoneName))
		ctx.Export("grafana_dns_record_id", grafanaRecord.ID())
		ctx.Export("grafana_url", pulumi.String("https://grafana."+sc.ZoneName))

		return nil
	})
}
