import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/svelte';
import ScheduleManagement from './ScheduleManagement.svelte';
import type { Schedule } from '../lib/types';

const { listSchedules, createSchedule, deleteSchedule, enableSchedule, disableSchedule } = vi.hoisted(() => ({
  listSchedules: vi.fn(),
  createSchedule: vi.fn(),
  deleteSchedule: vi.fn(),
  enableSchedule: vi.fn(),
  disableSchedule: vi.fn(),
}));

vi.mock('../lib/api', () => ({
  listSchedules,
  createSchedule,
  deleteSchedule,
  enableSchedule,
  disableSchedule,
}));

// 2024-01-01T02:00:00Z is 2023-12-31 21:00 (9:00 PM) EST in America/New_York.
const mockSchedule: Schedule = {
  name: 'hourly-job',
  cron_expression: '0 * * * *',
  def_name: 'process-orders',
  entry_point: '',
  input: '',
  enabled: true,
  next_run_at: '2024-01-01T02:00:00Z',
  created_at: '2024-01-01T00:00:00Z',
  updated_at: '2024-01-01T00:00:00Z',
  timezone: 'America/New_York',
};

async function openAddForm() {
  await fireEvent.click(screen.getByText('+ Add Schedule'));
}

describe('ScheduleManagement', () => {
  beforeEach(() => {
    vi.resetAllMocks();
    listSchedules.mockResolvedValue([mockSchedule]);
    createSchedule.mockResolvedValue(undefined);
  });

  it('shows each schedule\'s timezone in the table', async () => {
    render(ScheduleManagement);
    expect(await screen.findByText('America/New_York')).toBeTruthy();
  });

  it('renders next_run_at in the schedule\'s own timezone rather than silently as local time', async () => {
    render(ScheduleManagement);
    await screen.findByText('America/New_York');
    // Not just "some formatted date" -- specifically shifted into the
    // schedule's configured zone (EST, UTC-5) and labeled as such.
    expect(screen.getByText(/9:00:00\s*PM EST/)).toBeTruthy();
  });

  it('defaults an unset schedule to displaying UTC', async () => {
    listSchedules.mockResolvedValue([{ ...mockSchedule, timezone: '' }]);
    render(ScheduleManagement);
    expect(await screen.findByText('UTC')).toBeTruthy();
  });

  it('defaults the create-schedule timezone input to UTC', async () => {
    listSchedules.mockResolvedValue([]);
    render(ScheduleManagement);
    await screen.findByText('No schedules configured.');
    await openAddForm();
    const tzInput = screen.getByPlaceholderText('UTC') as HTMLInputElement;
    expect(tzInput.value).toBe('UTC');
  });

  it('offers a free-text timezone input (no hardcoded dropdown)', async () => {
    listSchedules.mockResolvedValue([]);
    render(ScheduleManagement);
    await screen.findByText('No schedules configured.');
    await openAddForm();
    const tzInput = screen.getByPlaceholderText('UTC') as HTMLInputElement;
    expect(tzInput.tagName).toBe('INPUT');
    expect(tzInput.type).toBe('text');
  });

  it('sends the entered timezone when creating a schedule', async () => {
    listSchedules.mockResolvedValue([]);
    render(ScheduleManagement);
    await screen.findByText('No schedules configured.');
    await openAddForm();

    await fireEvent.input(screen.getByPlaceholderText('my-schedule'), { target: { value: 'nightly' } });
    await fireEvent.input(screen.getByPlaceholderText('*/5 * * * *'), { target: { value: '0 0 * * *' } });
    await fireEvent.input(screen.getByPlaceholderText('PlaceOrder'), { target: { value: 'NightlyJob' } });
    await fireEvent.input(screen.getByPlaceholderText('UTC'), { target: { value: 'Europe/London' } });

    await fireEvent.click(screen.getByText('Create'));

    expect(createSchedule).toHaveBeenCalledWith(
      expect.objectContaining({
        name: 'nightly',
        cron: '0 0 * * *',
        def_name: 'NightlyJob',
        timezone: 'Europe/London',
      }),
    );
  });

  it('surfaces the server 400 message when the zone is rejected, rather than failing silently', async () => {
    listSchedules.mockResolvedValue([]);
    createSchedule.mockRejectedValue(new Error('invalid timezone: "Not/AZone"'));
    render(ScheduleManagement);
    await screen.findByText('No schedules configured.');
    await openAddForm();

    await fireEvent.input(screen.getByPlaceholderText('my-schedule'), { target: { value: 'bad-tz' } });
    await fireEvent.input(screen.getByPlaceholderText('*/5 * * * *'), { target: { value: '0 0 * * *' } });
    await fireEvent.input(screen.getByPlaceholderText('PlaceOrder'), { target: { value: 'Job' } });
    await fireEvent.input(screen.getByPlaceholderText('UTC'), { target: { value: 'Not/AZone' } });

    await fireEvent.click(screen.getByText('Create'));

    expect(await screen.findByText('invalid timezone: "Not/AZone"')).toBeTruthy();
    // The form stays open with the entered values on failure, rather than
    // silently discarding what the user typed.
    expect(screen.getByPlaceholderText('UTC')).toBeTruthy();
  });
});
