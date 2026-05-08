import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import SummaryCard from './SummaryCard.svelte';

describe('SummaryCard', () => {
  it('renders the count and label', () => {
    render(SummaryCard, { count: 42, label: 'Active' });
    expect(screen.getByText('42')).toBeTruthy();
    expect(screen.getByText('Active')).toBeTruthy();
  });

  it('renders zero count', () => {
    render(SummaryCard, { count: 0, label: 'Inactive' });
    expect(screen.getByText('0')).toBeTruthy();
    expect(screen.getByText('Inactive')).toBeTruthy();
  });

  it('renders with a custom color', () => {
    const { container } = render(SummaryCard, {
      count: 5,
      label: 'Failed',
      color: '#e63946',
    });
    const countEl = container.querySelector('.count') as HTMLElement;
    expect(countEl).toBeTruthy();
    expect(countEl.style.color).toBe('rgb(230, 57, 70)');
  });

  it('renders with default color when not specified', () => {
    const { container } = render(SummaryCard, { count: 10, label: 'Default' });
    const countEl = container.querySelector('.count') as HTMLElement;
    expect(countEl).toBeTruthy();
    expect(countEl.style.color).toBe('rgb(67, 97, 238)');
  });
});
