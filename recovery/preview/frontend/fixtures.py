"""Synthetic visual-preview data, never a backend or compatibility promise."""

import copy


USER = {
    "id": "preview-user", "username": "本地预览", "email": "preview@example.invalid",
    "role": "admin", "points": 0,
}
STATS = {
    "total_requests": 128, "successful_requests": 124, "failed_requests": 4,
    "total_points_consumed": 0, "success_rate": 96.875,
    "total_input_tokens": 18400, "total_output_tokens": 7200,
    "total_cache_creation_tokens": 0, "total_cache_read_tokens": 0,
    "avg_duration_ms": 1240, "avg_time_to_first_byte_ms": 180,
    "time_series": [], "model_distribution": [], "provider_distribution": [],
    "api_key_distribution": [], "user_distribution": [],
}
PROVIDERS = {
    "providers": [{
        "id": 1, "name": "本地预览 · Claude Code · 合成账号", "kind": "claude_code",
        "auth_type": "OAuth", "healthy": True, "weight": 1, "window_states": [],
        "last_error": None, "base_url": None, "http_proxy": None, "socks5_proxy": None,
        "rpm_limit": None, "rps_limit": None, "max_concurrent": 2,
        "active_concurrent": 0, "waiting_concurrent": 0, "available_from": None,
        "available_until": None, "last_checked_at": None, "created_at": None, "updated_at": None,
    }],
    "total": 1,
}
PROVIDER_ERRORS = {"types": [], "total": 1, "with_error": 0, "without_error": 1}
PROVIDER_WINDOWS = {
    "groups": [], "rate_limited_provider_count": 0, "rate_limited_earliest_recovery_at": None,
    "providers_at_concurrency_cap": 0, "high_load_provider_count": 0,
    "total_max_concurrent": 2, "total_active_concurrent": 0, "total_waiting_concurrent": 0,
    "concurrency_eligible_provider_count": 1, "concurrency_limited_provider_count": 1,
    "providers_with_active_windows": 0, "healthy_provider_count": 1,
    "active_window_count": 0, "total_rpm": 0, "total_tpm": 0,
}


def get_fixture(path):
    if path == "/portunex/users/me":
        return copy.deepcopy(USER)
    if path in {"/portunex/users/me/stats", "/portunex/admin/stats"}:
        return copy.deepcopy(STATS)
    if path == "/portunex/admin/providers":
        return copy.deepcopy(PROVIDERS)
    if path == "/portunex/admin/providers/error-types":
        return copy.deepcopy(PROVIDER_ERRORS)
    if path == "/portunex/admin/providers/window-summary":
        return copy.deepcopy(PROVIDER_WINDOWS)
    return None
