import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import StatusBadge from './StatusBadge.svelte';

describe('StatusBadge', () => {
  it('renders the status text for running', () => {
    render(StatusBadge, { status: 'running' });
    expect(screen.getByText('running')).toBeTruthy();
  });

  it('renders the status text for completed', () => {
    render(StatusBadge, { status: 'completed' });
    expect(screen.getByText('completed')).toBeTruthy();
  });

  it('renders the status text for failed', () => {
    render(StatusBadge, { status: 'failed' });
    expect(screen.getByText('failed')).toBeTruthy();
  });

  it('renders the status text for dead_letter', () => {
    render(StatusBadge, { status: 'dead_letter' });
    expect(screen.getByText('dead_letter')).toBeTruthy();
  });

  it('renders the status text for ready', () => {
    render(StatusBadge, { status: 'ready' });
    expect(screen.getByText('ready')).toBeTruthy();
  });

  it('renders a span with badge class', () => {
    const { container } = render(StatusBadge, { status: 'running' });
    const span = container.querySelector('span');
    expect(span).toBeTruthy();
    expect(span!.className).toContain('badge');
  });
});
