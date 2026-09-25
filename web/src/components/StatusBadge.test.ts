import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import StatusBadge from './StatusBadge.svelte';

// Every value workflow_instances.status can hold, and the class it must get.
//
// Asserting the CLASS is the whole point. The rendered text is `{status}`
// verbatim, so a test that asserts only the text passes for any string at all
// -- including one this component has never heard of. That is exactly how
// 'completed' and 'dead_letter', neither of which the engine writes, sat in the
// switch while these tests were green: every case passed, and the two statuses
// that actually occur (`done`, `dead_lettered`) fell through to no style.
const CASES: Array<[string, string]> = [
  ['ready', 'badge-ready'],
  ['running', 'badge-running'],
  ['done', 'badge-completed'],
  ['failed', 'badge-failed'],
  ['dead_lettered', 'badge-dead_letter'],
  ['terminated', 'badge-terminated'],
  ['cancelled', 'badge-cancelled'],
  ['terminating', 'badge-terminating'],
];

describe('StatusBadge', () => {
  it.each(CASES)('renders %s with class %s', (status, cls) => {
    const { container } = render(StatusBadge, { status });
    expect(screen.getByText(status)).toBeTruthy();
    expect(container.querySelector('span')!.className).toContain(cls);
  });

  // Negative control. 'completed' is not a workflow status, and before this
  // test existed it WAS styled -- so this case fails against the old switch.
  it('leaves a status the engine does not write unstyled', () => {
    const { container } = render(StatusBadge, { status: 'completed' });
    expect(container.querySelector('span')!.className.trim()).toBe('badge');
  });
});
