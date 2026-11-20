// Package cloudvm manages the lifecycle of the hub's cloud VM through the
// provider CLI (Azure: `az`): status, start, deallocate, restart, traffic and a
// cost estimate from the public retail price API, and guarded deletion of a
// resource group lazylongarm-tagged as the hub's own.
package cloudvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

var (
	commandContext = exec.CommandContext
	priceURL       = "https://prices.azure.com/api/retail/prices"
	httpClient     = &http.Client{Timeout: 20 * time.Second}
)

// Ref identifies a VM.
type Ref struct {
	Subscription  string
	ResourceGroup string
	Name          string
}

// VM is the subset of `az vm show -d` lazylongarm shows.
type VM struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Location     string            `json:"location"`
	Size         string            `json:"size"`
	Power        string            `json:"power"` // "VM running", "VM deallocated", ...
	Provisioning string            `json:"provisioning"`
	FQDNs        string            `json:"fqdns"`
	PublicIPs    string            `json:"publicIps"`
	PrivateIPs   string            `json:"privateIps"`
	DiskSKU      string            `json:"diskSku"`
	DiskGB       int               `json:"diskGb"`
	Tags         map[string]string `json:"tags"`
}

// Running reports whether the VM is powered on.
func (v VM) Running() bool { return strings.EqualFold(v.Power, "VM running") }

// Deallocated reports whether compute billing is stopped.
func (v VM) Deallocated() bool { return strings.EqualFold(v.Power, "VM deallocated") }

func az(ctx context.Context, v any, timeout time.Duration, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := commandContext(ctx, "az", append(args, "--only-show-errors")...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return errors.New("the Azure CLI (az) is not installed")
		}
		if ctx.Err() != nil {
			return fmt.Errorf("az %s: timed out after %s", strings.Join(args[:min(3, len(args))], " "), timeout)
		}
		msg := strings.TrimSpace(errb.String())
		if strings.Contains(msg, "az login") {
			return fmt.Errorf("Azure CLI is not logged in: run `az login`")
		}
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		if len(msg) > 240 {
			msg = msg[:240] + "…"
		}
		return fmt.Errorf("az %s: %s", strings.Join(args[:min(3, len(args))], " "), msg)
	}
	if v == nil {
		return nil
	}
	return json.Unmarshal(out.Bytes(), v)
}

func (r Ref) args() []string {
	return []string{"--subscription", r.Subscription, "--resource-group", r.ResourceGroup, "--name", r.Name}
}

// Show returns the VM with its power state.
func Show(ctx context.Context, r Ref) (VM, error) {
	var v VM
	args := append([]string{"vm", "show", "-d"}, r.args()...)
	args = append(args, "-o", "json", "--query", "{id:id, name:name, location:location, size:hardwareProfile.vmSize, power:powerState, provisioning:provisioningState, fqdns:fqdns, publicIps:publicIps, privateIps:privateIps, diskSku:storageProfile.osDisk.managedDisk.storageAccountType, diskGb:storageProfile.osDisk.diskSizeGB, tags:tags}")
	err := az(ctx, &v, 60*time.Second, args...)
	return v, err
}

// Action runs `az vm start|deallocate|restart`.
func Action(ctx context.Context, r Ref, verb string) error {
	switch verb {
	case "start", "deallocate", "restart":
	default:
		return fmt.Errorf("unsupported VM action %q", verb)
	}
	return az(ctx, nil, 10*time.Minute, append([]string{"vm", verb}, r.args()...)...)
}

// NetworkOut returns the VM's outbound bytes since start (Azure Monitor
// "Network Out Total", daily totals). The subscription is passed explicitly so
// az picks a token from that subscription's tenant (multi-tenant logins).
func NetworkOut(ctx context.Context, subscription, vmID string, start time.Time) (int64, error) {
	var resp struct {
		Value []struct {
			Timeseries []struct {
				Data []struct {
					Total *float64 `json:"total"`
				} `json:"data"`
			} `json:"timeseries"`
		} `json:"value"`
	}
	err := az(ctx, &resp, 60*time.Second, "monitor", "metrics", "list", "--subscription", subscription, "--resource", vmID, "--metrics", "Network Out Total",
		"--aggregation", "Total", "--interval", "PT24H", "--start-time", start.UTC().Format(time.RFC3339), "-o", "json")
	if err != nil {
		return 0, err
	}
	var sum float64
	for _, v := range resp.Value {
		for _, ts := range v.Timeseries {
			for _, d := range ts.Data {
				if d.Total != nil {
					sum += *d.Total
				}
			}
		}
	}
	return int64(sum), nil
}

// RGTags returns the resource group's tags.
func RGTags(ctx context.Context, sub, rg string) (map[string]string, error) {
	var tags map[string]string
	err := az(ctx, &tags, 60*time.Second, "group", "show", "--subscription", sub, "--name", rg, "-o", "json", "--query", "tags")
	return tags, err
}

// RGExists reports whether the resource group still exists.
func RGExists(ctx context.Context, sub, rg string) (bool, error) {
	var exists bool
	err := az(ctx, &exists, 60*time.Second, "group", "exists", "--subscription", sub, "--name", rg, "-o", "json")
	return exists, err
}

// OwnedRG reports whether tags mark the group as lazylongarm's hub group.
func OwnedRG(tags map[string]string) bool {
	return tags["managed-by"] == "lazylongarm" && tags["purpose"] == "longarm-hub"
}

// DeleteRG deletes a whole resource group (VM, disk, NIC, IP, NSG, VNet) and
// waits for it to finish.
func DeleteRG(ctx context.Context, sub, rg string) error {
	return az(ctx, nil, 30*time.Minute, "group", "delete", "--subscription", sub, "--name", rg, "--yes")
}

// DeleteArgs is the displayable delete command.
func DeleteArgs(sub, rg string) string {
	return fmt.Sprintf("az group delete --subscription %s --name %s --yes", sub, rg)
}

// Estimate is a monthly cost estimate in USD (retail list prices).
type Estimate struct {
	ComputeHourly float64  `json:"compute_hourly"`
	ComputeMonth  float64  `json:"compute_month"` // 730 h; 0 while deallocated
	IPMonth       float64  `json:"ip_month"`
	DiskMonth     float64  `json:"disk_month"`
	EgressMonth   float64  `json:"egress_month"` // beyond the free allowance, at EgressPerGB
	Total         float64  `json:"total"`
	Notes         []string `json:"notes"`
}

// EgressFreeGB and EgressPerGB approximate Azure's internet egress pricing
// (first 100 GB/month free; the next tier's rate varies by zone).
const (
	EgressFreeGB = 100.0
	EgressPerGB  = 0.12
)

// EstimateCost prices a VM from the public Azure retail price API.
func EstimateCost(ctx context.Context, v VM, egressBytes int64) (Estimate, error) {
	var e Estimate
	hourly, err := retail(ctx, fmt.Sprintf("armRegionName eq '%s' and armSkuName eq '%s' and priceType eq 'Consumption'", v.Location, v.Size), func(it priceItem) bool {
		return !strings.Contains(it.ProductName, "Windows") && !strings.Contains(it.SkuName, "Spot") && !strings.Contains(it.SkuName, "Low Priority") && it.UnitOfMeasure == "1 Hour"
	})
	if err != nil {
		return e, err
	}
	e.ComputeHourly = hourly
	if !v.Deallocated() {
		e.ComputeMonth = hourly * 730
	} else {
		e.Notes = append(e.Notes, "deallocated: no compute charge while stopped")
	}
	if ip, err := retail(ctx, fmt.Sprintf("armRegionName eq '%s' and priceType eq 'Consumption' and meterName eq 'Standard IPv4 Static Public IP'", v.Location), nil); err == nil {
		e.IPMonth = ip * 730
	}
	if tier := diskTier(v.DiskSKU, v.DiskGB); tier != "" {
		if d, err := retail(ctx, fmt.Sprintf("armRegionName eq '%s' and priceType eq 'Consumption' and meterName eq '%s'", v.Location, tier), nil); err == nil {
			e.DiskMonth = d
		}
	}
	gb := float64(egressBytes) / 1e9
	if gb > EgressFreeGB {
		e.EgressMonth = (gb - EgressFreeGB) * EgressPerGB
	}
	e.Total = e.ComputeMonth + e.IPMonth + e.DiskMonth + e.EgressMonth
	e.Notes = append(e.Notes, "retail list prices (credits and discounts not applied); egress beyond 100 GB at ~$0.12/GB")
	return e, nil
}

// diskTier maps a managed disk to its retail meter: Standard SSD E-tiers,
// Premium P-tiers, Standard HDD S-tiers (which start at S4 / 32 GiB).
func diskTier(sku string, gb int) string {
	tiers := []struct {
		gb   int
		name string
	}{{4, "1"}, {8, "2"}, {16, "3"}, {32, "4"}, {64, "6"}, {128, "10"}, {256, "15"}, {512, "20"}, {1024, "30"}}
	prefix := ""
	switch {
	case strings.HasPrefix(sku, "StandardSSD"):
		prefix = "E"
	case strings.HasPrefix(sku, "Premium"):
		prefix = "P"
	case strings.HasPrefix(sku, "Standard_"):
		prefix = "S"
		if gb <= 32 {
			return "S4 LRS Disk"
		}
	default:
		return ""
	}
	for _, t := range tiers {
		if gb <= t.gb {
			return prefix + t.name + " LRS Disk"
		}
	}
	return ""
}

type priceItem struct {
	RetailPrice   float64 `json:"retailPrice"`
	UnitOfMeasure string  `json:"unitOfMeasure"`
	ProductName   string  `json:"productName"`
	SkuName       string  `json:"skuName"`
	MeterName     string  `json:"meterName"`
}

func retail(ctx context.Context, filter string, keep func(priceItem) bool) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, priceURL+"?$filter="+url.QueryEscape(filter), nil)
	if err != nil {
		return 0, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("Azure price API: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Items []priceItem `json:"Items"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return 0, err
	}
	for _, it := range body.Items {
		if keep == nil || keep(it) {
			return it.RetailPrice, nil
		}
	}
	return 0, fmt.Errorf("no retail price for %s", filter)
}
