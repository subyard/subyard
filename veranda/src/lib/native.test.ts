import { expect, it, vi } from 'vitest';
import { reportFrontendDiagnostic } from './native';

const invoke = vi.hoisted(() => vi.fn().mockRejectedValue(new Error('native unavailable')));
vi.mock('@tauri-apps/api/core', () => ({ invoke }));

it('sends each fixed frontend diagnostic once and contains native-report failures', async () => {
    for (let attempt = 0; attempt < 100; attempt++) {
        reportFrontendDiagnostic('boundary');
        reportFrontendDiagnostic('error');
        reportFrontendDiagnostic('unhandled_rejection');
    }
    await Promise.resolve();
    expect(invoke.mock.calls).toEqual([
        ['frontend_diagnostic', { event: 'boundary' }],
        ['frontend_diagnostic', { event: 'error' }],
        ['frontend_diagnostic', { event: 'unhandled_rejection' }]
    ]);
});
