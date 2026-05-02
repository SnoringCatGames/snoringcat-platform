package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pulumi/pulumi-cloudflare/sdk/v5/go/cloudflare"
	"github.com/pulumi/pulumi-hcloud/sdk/go/hcloud"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

const (
	zoneName = "snoringcat.games"
	location = "hil"
	// CAX (ARM) isn't available in Hillsboro, and CX (Intel) is
	// EU-only too. Hillsboro offers CPX (AMD shared) and CCX (AMD
	// dedicated). CPX21 (3 vCPU / 4 GB / 80 GB) is the closest match
	// to the originally-planned CAX11 spec (4 GB RAM target). Cost:
	// ~€7.05/mo per box, total ~€14/mo for the pair (vs CAX11's
	// ~€7.60/mo total — the US location premium is real but
	// Hillsboro is required for North American game latency).
	serverType        = "cpx21"
	image             = "ubuntu-24.04"
	networkIPRange    = "10.0.0.0/16"
	subnetIPRange     = "10.0.1.0/24"
	nakamaPrivateIP   = "10.0.1.10"
	postgresPrivateIP = "10.0.1.20"
	networkZone       = "us-west"
)

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
  - systemctl enable --now fail2ban
`

const nakamaExtraRuncmd = `  - ufw allow 80/tcp
  - ufw allow 443/tcp
  - ufw --force enable
  - touch /var/lib/cloud/snoringcat-bootstrap-done
`

const postgresExtraRuncmd = `  - ufw --force enable
  - touch /var/lib/cloud/snoringcat-bootstrap-done
`

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		cfg := config.New(ctx, "")
		adminCidr := cfg.Get("adminCidr")

		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("user home dir: %w", err)
		}
		nakamaPubPath := cfg.Get("nakamaSshPubkeyPath")
		if nakamaPubPath == "" {
			nakamaPubPath = filepath.Join(home, ".hopnbop-migration", "ssh", "nakama.pub")
		}
		postgresPubPath := cfg.Get("postgresSshPubkeyPath")
		if postgresPubPath == "" {
			postgresPubPath = filepath.Join(home, ".hopnbop-migration", "ssh", "postgres.pub")
		}

		nakamaPub, err := os.ReadFile(nakamaPubPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", nakamaPubPath, err)
		}
		postgresPub, err := os.ReadFile(postgresPubPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", postgresPubPath, err)
		}

		zone, err := cloudflare.LookupZone(ctx, &cloudflare.LookupZoneArgs{
			Name: pulumi.StringRef(zoneName),
		})
		if err != nil {
			return fmt.Errorf("lookup cloudflare zone %s: %w", zoneName, err)
		}

		nakamaKey, err := hcloud.NewSshKey(ctx, "nakama", &hcloud.SshKeyArgs{
			Name:      pulumi.String("nakama"),
			PublicKey: pulumi.String(strings.TrimSpace(string(nakamaPub))),
		})
		if err != nil {
			return err
		}
		postgresKey, err := hcloud.NewSshKey(ctx, "postgres", &hcloud.SshKeyArgs{
			Name:      pulumi.String("postgres"),
			PublicKey: pulumi.String(strings.TrimSpace(string(postgresPub))),
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
			NetworkZone: pulumi.String(networkZone),
			IpRange:     pulumi.String(subnetIPRange),
		})
		if err != nil {
			return err
		}

		nakamaUserData := baseCloudInit + nakamaExtraRuncmd
		postgresUserData := baseCloudInit + postgresExtraRuncmd

		nakamaSrv, err := hcloud.NewServer(ctx, "nakama-prod-1", &hcloud.ServerArgs{
			Name:       pulumi.String("nakama-prod-1"),
			ServerType: pulumi.String(serverType),
			Image:      pulumi.String(image),
			Location:   pulumi.String(location),
			SshKeys:    pulumi.StringArray{nakamaKey.Name},
			UserData:   pulumi.String(nakamaUserData),
			Networks: hcloud.ServerNetworkTypeArray{
				&hcloud.ServerNetworkTypeArgs{
					NetworkId: idToInt(network.ID()),
					Ip:        pulumi.String(nakamaPrivateIP),
				},
			},
		}, pulumi.DependsOn([]pulumi.Resource{subnet}))
		if err != nil {
			return err
		}

		postgresSrv, err := hcloud.NewServer(ctx, "postgres-prod-1", &hcloud.ServerArgs{
			Name:       pulumi.String("postgres-prod-1"),
			ServerType: pulumi.String(serverType),
			Image:      pulumi.String(image),
			Location:   pulumi.String(location),
			SshKeys:    pulumi.StringArray{postgresKey.Name},
			UserData:   pulumi.String(postgresUserData),
			Networks: hcloud.ServerNetworkTypeArray{
				&hcloud.ServerNetworkTypeArgs{
					NetworkId: idToInt(network.ID()),
					Ip:        pulumi.String(postgresPrivateIP),
				},
			},
		}, pulumi.DependsOn([]pulumi.Resource{subnet}))
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
		if adminCidr != "" {
			adminSshSources = pulumi.StringArray{pulumi.String(adminCidr)}
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

		_, err = hcloud.NewFirewall(ctx, "postgres-fw", &hcloud.FirewallArgs{
			Name: pulumi.String("postgres-fw"),
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
					Port:      pulumi.String("5432"),
					SourceIps: pulumi.StringArray{pulumi.String(nakamaPrivateIP + "/32")},
				},
			},
			ApplyTos: hcloud.FirewallApplyToArray{
				&hcloud.FirewallApplyToArgs{
					Server: idToInt(postgresSrv.ID()),
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

		// Grafana subdomain. Same Nakama box (Caddy reverse-proxies
		// grafana.snoringcat.games:443 → grafana:3000 internally).
		grafanaRecord, err := cloudflare.NewRecord(ctx, "grafana-a", &cloudflare.RecordArgs{
			ZoneId:  pulumi.String(zone.Id),
			Name:    pulumi.String("grafana"),
			Type:    pulumi.String("A"),
			Content: nakamaSrv.Ipv4Address,
			Proxied: pulumi.Bool(false),
			Ttl:     pulumi.Int(1),
			Comment: pulumi.String("Phase B: Grafana ops dashboard"),
		})
		if err != nil {
			return err
		}

		ctx.Export("nakama_server_id", nakamaSrv.ID())
		ctx.Export("nakama_public_ip", nakamaSrv.Ipv4Address)
		ctx.Export("nakama_private_ip", pulumi.String(nakamaPrivateIP))
		ctx.Export("postgres_server_id", postgresSrv.ID())
		ctx.Export("postgres_public_ip", postgresSrv.Ipv4Address)
		ctx.Export("postgres_private_ip", pulumi.String(postgresPrivateIP))
		ctx.Export("private_network_id", network.ID())
		ctx.Export("zone_id", pulumi.String(zone.Id))
		ctx.Export("nakama_dns_record_id", nakamaRecord.ID())
		ctx.Export("grafana_dns_record_id", grafanaRecord.ID())
		ctx.Export("nakama_url", pulumi.String("https://nakama."+zoneName))
		ctx.Export("grafana_url", pulumi.String("https://grafana."+zoneName))

		return nil
	})
}
