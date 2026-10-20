package proxy

func usesQwenErrorPatch(target *Target) bool {
	return targetVendor(target) == usageAdapterQwen
}

func qwenKeyExhausted(status int, fields upstreamErrorFields) (bool, string) {
	switch fields.Code {
	case "Arrearage":
		return true, "Arrearage"
	case "AccessDenied.Unpurchased":
		return true, "AccessDenied.Unpurchased"
	case "AllocationQuota.FreeTierOnly":
		return true, "free_tier_exhausted"
	case "PrepaidBillOverdue", "PostpaidBillOverdue", "CommodityNotPurchased", "QuotaExceeded":
		return true, "quota_exceeded"
	}
	// In particular, insufficient_quota and Throttling.AllocationQuota describe
	// TPS/TPM limits in Alibaba's API and do not disable the key.
	return false, ""
}
