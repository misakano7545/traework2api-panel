// credit — TRAE SOLO 积分查询（全部账号 + 指定账号 + 总计）。
//
// 用法:
//
//	go run ./cmd/credit             # 或编译后 ./credit
//	./credit.sh                     # 美化日报
//	./credit.sh -json               # 原始 JSON
//	./credit.sh <uid>               # 指定账号
//
// 数据源：POST {ug}/trae/api/v2/pay/ide_user_ent_usage，
// 聚合 user_entitlement_pack_list[].entitlement_base_info.quota.credits_limit。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"traework2api/internal/upstream"
)

type authFile struct {
	Auth struct {
		AccessToken string `json:"accessToken"`
		Domain      string `json:"domain"`
		DeviceID    string `json:"deviceId"`
	} `json:"auth"`
	Account struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
	} `json:"account"`
}

type accountResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Remain   *int64 `json:"remain"`
	Limit    *int64 `json:"limit,omitempty"`
	Packages int    `json:"packages,omitempty"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

func fetchEntUsage(af *authFile) (remain int64, limit int64, packs int, err error) {
	req, err := http.NewRequest(http.MethodPost, upstream.UgHost+upstream.EpEntUsage, bytes.NewReader([]byte("{}")))
	if err != nil {
		return 0, 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+af.Auth.AccessToken)
	req.Header.Set("X-User-Region", "CN")
	if af.Auth.DeviceID != "" {
		req.Header.Set("X-Device-Id", af.Auth.DeviceID)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, 0, 0, fmt.Errorf("http %d", resp.StatusCode)
	}
	var env struct {
		IsCreditsBilling        bool `json:"is_credits_billing"`
		UserEntitlementPackList []struct {
			EntitlementBaseInfo struct {
				Quota struct {
					CreditsLimit int64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			// usage.credits_amount 是已用积分(实测),remain = limit - used
			Usage struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return 0, 0, 0, err
	}
	for _, p := range env.UserEntitlementPackList {
		l := p.EntitlementBaseInfo.Quota.CreditsLimit
		if l <= 0 {
			continue
		}
		used := int64(p.Usage.CreditsAmount)
		limit += l
		remain += l - used
	}
	return remain, limit, len(env.UserEntitlementPackList), nil
}

func main() {
	args := os.Args[1:]
	pretty := false
	var wantUID string
	for _, a := range args {
		switch a {
		case "-pretty":
			pretty = true
		case "-json":
			// 默认 JSON 输出
		default:
			if !strings.HasPrefix(a, "-") {
				wantUID = a
			}
		}
	}
	authDir := "./auths"
	if v := os.Getenv("TW2A_AUTH_DIR"); v != "" {
		authDir = v
	}
	files, _ := filepath.Glob(filepath.Join(authDir, "trae-*.json"))
	sort.Strings(files)

	accounts := make([]accountResult, 0, len(files))
	for _, f := range files {
		var af authFile
		raw, err := os.ReadFile(f)
		if err != nil || json.Unmarshal(raw, &af) != nil {
			continue
		}
		if wantUID != "" && af.Account.UID != wantUID {
			continue
		}
		res := accountResult{UID: af.Account.UID, Nickname: af.Account.Nickname}
		if af.Auth.AccessToken == "" {
			res.Error = "no accessToken"
			accounts = append(accounts, res)
			continue
		}
		remain, limit, packs, err := fetchEntUsage(&af)
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Remain = &remain
			res.Limit = &limit
			res.Packages = packs
			res.OK = true
		}
		accounts = append(accounts, res)
		time.Sleep(200 * time.Millisecond)
	}

	var totalRemain int64
	var totalLimit int64
	okCount := 0
	for _, a := range accounts {
		if a.OK && a.Remain != nil {
			okCount++
			totalRemain += *a.Remain
			if a.Limit != nil {
				totalLimit += *a.Limit
			}
		}
	}
	out := map[string]any{
		"service": "traework2api",
		"ts":      time.Now().Unix(),
		"total": map[string]any{
			"remain":   totalRemain,
			"limit":    totalLimit,
			"accounts": len(accounts),
			"ok":       okCount,
			"failed":   len(accounts) - okCount,
		},
		"accounts": accounts,
	}
	if pretty {
		printPretty(accounts, totalRemain, okCount)
		return
	}
	raw, _ := json.Marshal(out)
	fmt.Println(string(raw))
}

func printPretty(accounts []accountResult, totalRemain int64, okCount int) {
	withBalance := 0
	var failed []string
	for _, a := range accounts {
		if a.OK && a.Remain != nil && *a.Remain > 0 {
			withBalance++
		}
		if !a.OK {
			name := a.Nickname
			if name == "" && len(a.UID) >= 8 {
				name = a.UID[:8]
			}
			failed = append(failed, name+" "+a.Error)
		}
	}
	fmt.Printf("📊 TRAE SOLO 积分日报\n")
	fmt.Printf("账号: %d/%d\n", withBalance, len(accounts))
	fmt.Printf("总计: %d\n", totalRemain)
	for _, f := range failed {
		fmt.Printf("⚠️ %s\n", f)
	}
}
