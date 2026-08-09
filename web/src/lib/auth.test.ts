import { describe, it, expect, beforeEach, vi } from 'vitest';
import { getToken, setToken, clearToken, notifyUnauthorized, onUnauthorized } from './auth';

describe('auth token storage', () => {
  beforeEach(() => {
    localStorage.clear();
  });

  it('returns an empty string when nothing is stored', () => {
    expect(getToken()).toBe('');
  });

  it('persists a token to localStorage under the cleat_api_token key', () => {
    setToken('cleat_sk_abc123');
    expect(getToken()).toBe('cleat_sk_abc123');
    expect(localStorage.getItem('cleat_api_token')).toBe('cleat_sk_abc123');
  });

  it('removes the stored token when set to an empty string', () => {
    setToken('cleat_sk_abc123');
    setToken('');
    expect(getToken()).toBe('');
    expect(localStorage.getItem('cleat_api_token')).toBeNull();
  });

  it('clearToken removes any stored token', () => {
    setToken('cleat_sk_abc123');
    clearToken();
    expect(getToken()).toBe('');
  });
});

describe('unauthorized notifications', () => {
  beforeEach(() => {
    localStorage.clear();
  });

  it('notifyUnauthorized clears the stored token', () => {
    setToken('cleat_sk_stale');
    notifyUnauthorized();
    expect(getToken()).toBe('');
  });

  it('notifyUnauthorized invokes subscribers registered via onUnauthorized', () => {
    const handler = vi.fn();
    const off = onUnauthorized(handler);
    notifyUnauthorized();
    expect(handler).toHaveBeenCalledTimes(1);
    off();
  });

  it('the unsubscribe function returned by onUnauthorized stops delivery', () => {
    const handler = vi.fn();
    const off = onUnauthorized(handler);
    off();
    notifyUnauthorized();
    expect(handler).not.toHaveBeenCalled();
  });
});
