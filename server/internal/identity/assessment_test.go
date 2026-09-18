package identity

import "testing"

func TestAssessmentWebApprovalRequiresExplicitPairing(t *testing.T) {
	for _, profile := range []PairingProfile{PairingProfileUser, PairingProfileAgent, PairingProfileReferences, PairingProfileImport, PairingProfileAssessment} {
		scopes, err := pairingProfileScopes(profile)
		if err != nil {
			t.Fatal(err)
		}
		web := WebScopes(scopes)
		if hasScope(web, "learning:approve") != (profile == PairingProfileAssessment) {
			t.Fatalf("审批权限错误：%s %v", profile, web)
		}
		if profile == PairingProfileAssessment {
			for _, forbidden := range []string{"devices:manage", "knowledge:approve", "knowledge:write", "settings:write", "privacy:device"} {
				if hasScope(web, forbidden) {
					t.Fatalf("评估档案意外包含权限：%s", forbidden)
				}
			}
		}
	}
}
