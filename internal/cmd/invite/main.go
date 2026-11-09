// Command invite 管理 Trae 邀请码与新手/活动福利。
//
// 用法：
//
//	invite                        # 查看所有国内账号的邀请码与专属链接
//	invite -uid <uid>             # 查看指定账号的邀请码
//	invite -redeem <code>         # 为所有适用账号兑换指定邀请码
//	invite -redeem <code> -uid X  # 为指定账号兑换邀请码
//	invite -newbie                # 为所有国内账号领取新手/活动积分
//	invite -newbie -uid X         # 为指定账号领取新手/活动积分
package invite

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"trae2api/internal/cli"
)

// Run 执行 invite 子命令，返回退出码（由总入口 os.Exit）。
func Run(args []string) int {
	return cli.RunArgs(func() error { return run(args) })
}

func run(args []string) error {
	fs := flag.NewFlagSet("invite", flag.ContinueOnError)
	base, key := cli.GatewayFlags(fs)
	uid := fs.String("uid", "", "指定目标账号 UID")
	redeemCode := fs.String("redeem", "", "兑换指定邀请码（如 -redeem FRIEND123）")
	claimNewbie := fs.Bool("newbie", false, "领取新手与首发消息活动奖励")
	asJSON := fs.Bool("json", false, "以 JSON 格式输出")

	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `invite —— Trae 邀请码与活动福利管理

用法:
  invite [flags]                           查询账号邀请码与奖励
  invite -redeem <code> [flags]            兑换邀请码获取赠送积分
  invite -newbie [flags]                   一键领取新手与首发消息福利

选项:
  -uid string      指定单个账号 UID（默认作用于全部国内账号）
  -redeem string   要兑换的邀请码
  -newbie          领取新手/活动奖励
  -json            以 JSON 格式输出
  -url string      网关基础地址 (默认从环境变量 TRAE2API_URL 或 http://127.0.0.1:7870)
  -key string      网关 API 密钥 (默认从环境变量 TRAE2API_KEY)
`)
	}

	if err := fs.Parse(args); err != nil {
		return cli.Usagef("%v", err)
	}

	c := cli.NewClient(*base, *key)

	if *claimNewbie {
		return runClaimNewbie(c, *uid, *asJSON)
	}

	if strings.TrimSpace(*redeemCode) != "" {
		return runRedeemInvite(c, strings.TrimSpace(*redeemCode), *uid, *asJSON)
	}

	return runListInviteCodes(c, *uid, *asJSON)
}

type statusAccount struct {
	UID      string `json:"uid"`
	Label    string `json:"label"`
	Realm    string `json:"realm"`
	Disabled bool   `json:"disabled"`
}

func fetchCandidateAccounts(c *cli.Client, targetUID string) ([]statusAccount, error) {
	var resp struct {
		Accounts []statusAccount `json:"accounts"`
	}
	if err := c.Get("/status", &resp); err != nil {
		return nil, err
	}
	var out []statusAccount
	for _, a := range resp.Accounts {
		if targetUID != "" && a.UID != targetUID {
			continue
		}
		// 过滤国际版与被停用账号（如果指定了 uid 则不强行排除）
		if targetUID == "" && (a.Realm == "international" || a.Realm == "solo" || a.Disabled) {
			continue
		}
		out = append(out, a)
	}
	if targetUID != "" && len(out) == 0 {
		out = append(out, statusAccount{UID: targetUID, Label: targetUID})
	}
	return out, nil
}

func runListInviteCodes(c *cli.Client, targetUID string, asJSON bool) error {
	accts, err := fetchCandidateAccounts(c, targetUID)
	if err != nil {
		return err
	}
	if len(accts) == 0 {
		fmt.Println("没有找到适用的国内 Trae 账号。")
		return nil
	}

	type inviteRow struct {
		UID           string `json:"uid"`
		Label         string `json:"label"`
		InviteCode    string `json:"invite_code"`
		RewardCredits int64  `json:"reward_credits"`
		InviteURL     string `json:"invite_url"`
		Status        string `json:"status"`
	}

	rows := make([]inviteRow, 0, len(accts))
	for _, a := range accts {
		var res struct {
			OK            bool   `json:"ok"`
			UID           string `json:"uid"`
			Label         string `json:"label"`
			InviteCode    string `json:"invite_code"`
			InviteURL     string `json:"invite_url"`
			RewardCredits int64  `json:"reward_credits"`
			Message       string `json:"message"`
		}
		err := c.Get("/admin/accounts/"+a.UID+"/invite-code", &res)
		if err != nil {
			rows = append(rows, inviteRow{
				UID:    a.UID,
				Label:  a.Label,
				Status: err.Error(),
			})
			continue
		}
		rows = append(rows, inviteRow{
			UID:           a.UID,
			Label:         res.Label,
			InviteCode:    res.InviteCode,
			RewardCredits: res.RewardCredits,
			InviteURL:     res.InviteURL,
			Status:        "成功",
		})
	}

	if asJSON {
		cli.JSON(rows)
		return nil
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].UID < rows[j].UID })
	t := &cli.Table{Headers: []string{"UID", "备注/标签", "邀请码", "单次奖励", "邀请链接", "状态"}}
	for _, r := range rows {
		rewardStr := "-"
		if r.RewardCredits > 0 {
			rewardStr = fmt.Sprintf("%d 积分", r.RewardCredits)
		}
		codeStr := r.InviteCode
		if codeStr == "" {
			codeStr = "-"
		}
		urlStr := r.InviteURL
		if urlStr == "" {
			urlStr = "-"
		}
		t.Add(r.UID, r.Label, codeStr, rewardStr, urlStr, r.Status)
	}
	fmt.Print(t.Render())
	return nil
}

func runRedeemInvite(c *cli.Client, code, targetUID string, asJSON bool) error {
	accts, err := fetchCandidateAccounts(c, targetUID)
	if err != nil {
		return err
	}
	if len(accts) == 0 {
		return fmt.Errorf("没有找到适用的账号来兑换邀请码")
	}

	type redeemRow struct {
		UID     string `json:"uid"`
		Label   string `json:"label"`
		Success bool   `json:"success"`
		Credits int64  `json:"credits"`
		Message string `json:"message"`
	}

	rows := make([]redeemRow, 0, len(accts))
	for _, a := range accts {
		body := map[string]string{"invite_code": code}
		var res struct {
			OK      bool   `json:"ok"`
			UID     string `json:"uid"`
			Credits int64  `json:"credits"`
			Message string `json:"message"`
		}
		err := c.Post("/admin/accounts/"+a.UID+"/redeem-invite", body, &res)
		if err != nil {
			rows = append(rows, redeemRow{
				UID:     a.UID,
				Label:   a.Label,
				Success: false,
				Message: err.Error(),
			})
			continue
		}
		rows = append(rows, redeemRow{
			UID:     a.UID,
			Label:   a.Label,
			Success: res.OK,
			Credits: res.Credits,
			Message: res.Message,
		})
	}

	if asJSON {
		cli.JSON(rows)
		return nil
	}

	t := &cli.Table{Headers: []string{"UID", "备注/标签", "兑换结果", "获得积分", "说明"}}
	for _, r := range rows {
		status := "成功"
		if !r.Success {
			status = "失败"
		}
		creditsStr := "-"
		if r.Credits > 0 {
			creditsStr = fmt.Sprintf("+%d", r.Credits)
		}
		t.Add(r.UID, r.Label, status, creditsStr, r.Message)
	}
	fmt.Print(t.Render())
	return nil
}

func runClaimNewbie(c *cli.Client, targetUID string, asJSON bool) error {
	if targetUID != "" {
		var res struct {
			OK      bool   `json:"ok"`
			UID     string `json:"uid"`
			Results []struct {
				ActivityID int    `json:"activity_id"`
				Name       string `json:"name"`
				Success    bool   `json:"success"`
				Credits    int64  `json:"credits"`
				Message    string `json:"message"`
			} `json:"results"`
		}
		if err := c.Post("/admin/accounts/"+targetUID+"/claim-newbie", nil, &res); err != nil {
			return err
		}
		if asJSON {
			cli.JSON(res)
			return nil
		}
		t := &cli.Table{Headers: []string{"活动", "状态", "积分", "说明"}}
		for _, it := range res.Results {
			status := "成功"
			if !it.Success {
				status = "已领/失败"
			}
			t.Add(it.Name, status, fmt.Sprintf("%d", it.Credits), it.Message)
		}
		fmt.Print(t.Render())
		return nil
	}

	// 批量领取全部
	var resp []struct {
		UID     string `json:"uid"`
		Label   string `json:"label"`
		Results []struct {
			ActivityID int    `json:"activity_id"`
			Name       string `json:"name"`
			Success    bool   `json:"success"`
			Credits    int64  `json:"credits"`
			Message    string `json:"message"`
		} `json:"results"`
		Error string `json:"error,omitempty"`
	}
	if err := c.Post("/admin/accounts/claim-all-newbie", nil, &resp); err != nil {
		return err
	}
	if asJSON {
		cli.JSON(resp)
		return nil
	}

	if len(resp) == 0 {
		fmt.Println("没有适用的账号。")
		return nil
	}

	t := &cli.Table{Headers: []string{"UID", "备注/标签", "活动项目", "状态", "积分", "详情"}}
	for _, a := range resp {
		if a.Error != "" {
			t.Add(a.UID, a.Label, "-", "失败", "-", a.Error)
			continue
		}
		for _, it := range a.Results {
			status := "成功"
			if !it.Success {
				status = "已领/跳过"
			}
			creditsStr := "-"
			if it.Credits > 0 {
				creditsStr = fmt.Sprintf("+%d", it.Credits)
			}
			t.Add(a.UID, a.Label, it.Name, status, creditsStr, it.Message)
		}
	}
	fmt.Print(t.Render())
	return nil
}
