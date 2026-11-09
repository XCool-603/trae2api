package traectl

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// pathGetInviteCode 获取账号邀请码。
	pathGetInviteCode = "/trae/api/v1/pay/get_invite_code"
	// pathRedeemInviteCode 兑换邀请码（新手/好友绑定）。
	pathRedeemInviteCode = "/trae/api/v1/pay/redeem_invite_code"
	// pathActivityInfo 查询商业活动列表。
	pathActivityInfo = "/trae/api/v2/ug/activity/info"
	// pathActivityAction 参与/领取活动权益。
	pathActivityAction = "/trae/api/v2/ug/activity/action"
)

// InviteCodeInfo 邀请码信息。
type InviteCodeInfo struct {
	InviteCode    string `json:"invite_code"`
	InviteURL     string `json:"invite_url,omitempty"`
	RewardCredits int64  `json:"reward_credits,omitempty"`
	Message       string `json:"message,omitempty"`
}

// RedeemInviteResult 兑换邀请码结果。
type RedeemInviteResult struct {
	Success bool   `json:"success"`
	Credits int64  `json:"credits,omitempty"`
	Message string `json:"message"`
}

// ActivityBonusResult 单项活动福利领取结果。
type ActivityBonusResult struct {
	ActivityID   int    `json:"activity_id"`
	ActivityName string `json:"activity_name"`
	Success      bool   `json:"success"`
	Message      string `json:"message"`
}

// GetInviteCode 获取当前账号的邀请码（需资格）。
func (s *Store) GetInviteCode(ctx context.Context) (*InviteCodeInfo, error) {
	if s.skipCheckin() {
		return nil, fmt.Errorf("traectl: 国际版或 SOLO 账号不支持邀请码体系")
	}

	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			InviteCode    string `json:"invite_code"`
			InviteURL     string `json:"invite_url"`
			RewardCredits int64  `json:"reward_credits"`
		} `json:"data"`
	}

	if err := s.postUGBody(ctx, pathGetInviteCode, []byte(`{}`), &resp); err != nil {
		return nil, err
	}

	if resp.Code != 0 && resp.Code != 200 {
		msg := resp.Message
		if resp.Code == 9019 {
			msg = "暂无邀请资格或活动未开启 (code=9019)"
		}
		return nil, fmt.Errorf("traectl: 获取邀请码失败 code=%d: %s", resp.Code, msg)
	}

	code := resp.Data.InviteCode
	if code == "" && resp.Data.InviteURL != "" {
		// 部分情况下邀请码在 URL query 中
		if idx := strings.Index(resp.Data.InviteURL, "code="); idx >= 0 {
			code = resp.Data.InviteURL[idx+5:]
			if am := strings.IndexByte(code, '&'); am >= 0 {
				code = code[:am]
			}
		}
	}

	return &InviteCodeInfo{
		InviteCode:    code,
		InviteURL:     resp.Data.InviteURL,
		RewardCredits: resp.Data.RewardCredits,
		Message:       resp.Message,
	}, nil
}

// RedeemInviteCode 在当前账号上兑换/绑定指定邀请码。
func (s *Store) RedeemInviteCode(ctx context.Context, inviteCode string) (*RedeemInviteResult, error) {
	if s.skipCheckin() {
		return nil, fmt.Errorf("traectl: 国际版或 SOLO 账号不支持兑换邀请码")
	}

	inviteCode = strings.TrimSpace(inviteCode)
	if inviteCode == "" {
		return nil, fmt.Errorf("traectl: 邀请码不能为空")
	}

	body, err := json.Marshal(map[string]any{
		"invite_code": inviteCode,
		"req_source":  1,
	})
	if err != nil {
		return nil, err
	}

	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Credits int64 `json:"credits"`
		} `json:"data"`
	}

	if err := s.postUGBody(ctx, pathRedeemInviteCode, body, &resp); err != nil {
		return nil, err
	}

	if resp.Code != 0 && resp.Code != 200 {
		msg := resp.Message
		if resp.Code == 9030 {
			msg = "邀请码无效或已过期 (code=9030)"
		} else if resp.Code == 9019 {
			msg = "当前账号不满足兑换资格（例如已非新用户）(code=9019)"
		}
		return &RedeemInviteResult{
			Success: false,
			Message: fmt.Sprintf("兑换失败 code=%d: %s", resp.Code, msg),
		}, nil
	}

	return &RedeemInviteResult{
		Success: true,
		Credits: resp.Data.Credits,
		Message: "兑换成功！已获得邀请奖励福利",
	}, nil
}

// ClaimActivityBonus 领取指定商业活动福利（如 102 新用户积分，105 发消息奖励等）。
func (s *Store) ClaimActivityBonus(ctx context.Context, activityID int, activityName string) (*ActivityBonusResult, error) {
	if s.skipCheckin() {
		return nil, fmt.Errorf("traectl: 国际版或 SOLO 账号不支持活动体系")
	}

	body, err := json.Marshal(map[string]any{
		"activity_id": activityID,
		"req_source":  1,
	})
	if err != nil {
		return nil, err
	}

	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	if err := s.postUGBody(ctx, pathActivityAction, body, &resp); err != nil {
		return nil, err
	}

	if resp.Code != 0 && resp.Code != 200 {
		return &ActivityBonusResult{
			ActivityID:   activityID,
			ActivityName: activityName,
			Success:      false,
			Message:      fmt.Sprintf("领取失败 code=%d: %s", resp.Code, resp.Message),
		}, nil
	}

	return &ActivityBonusResult{
		ActivityID:   activityID,
		ActivityName: activityName,
		Success:      true,
		Message:      "领取成功！",
	}, nil
}

// ClaimNewbieBonus 一键领取常见的新手福利及首登活动积分（活动 102 与 105）。
func (s *Store) ClaimNewbieBonus(ctx context.Context) ([]ActivityBonusResult, error) {
	if s.skipCheckin() {
		return nil, fmt.Errorf("traectl: 国际版或 SOLO 账号不支持新手福利领取")
	}

	activities := []struct {
		id   int
		name string
	}{
		{id: 102, name: "新用户积分 (new_user_credits)"},
		{id: 105, name: "首登发消息奖励 (send_message_reward)"},
	}

	results := make([]ActivityBonusResult, 0, len(activities))
	for _, act := range activities {
		res, err := s.ClaimActivityBonus(ctx, act.id, act.name)
		if err != nil {
			results = append(results, ActivityBonusResult{
				ActivityID:   act.id,
				ActivityName: act.name,
				Success:      false,
				Message:      err.Error(),
			})
			continue
		}
		results = append(results, *res)
	}

	return results, nil
}
