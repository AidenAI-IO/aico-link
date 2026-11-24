import { apiError } from '../api/client';
import type { ArchiveHeader } from './transfers';
export interface BackupJob {
  job_id: string;
  state: string;
  kind?: string;
  phase?: string;
  archive_size?: number;
  estimated_bytes?: number;
  bytes_read?: number;
  received_bytes?: number;
  archive_url?: string;
  suggested_filename?: string;
  chunk_size?: number;
  plan_digest?: string;
  manifest?: { components: { id: string }[] };
  warnings?: string[];
  error?: { code?: string; message?: string };
}
export interface BackupView {
  kind: 'backup' | 'restore' | null;
  running: boolean;
  stalled: boolean;
  phase: string;
  done: number;
  total: number;
  message: string;
  error: boolean;
  finished: boolean;
  rebootRequired: boolean;
}
interface Session {
  expires_at: string;
  csrf_token: string;
}
/**
 * Full data backup and restore, without any DOM.
 *
 * The protocol is the classic page's (config/backup.js), carried over step for
 * step: a maintenance session with a CSRF token, job creation, streaming the
 * archive through the browser's download manager, chunked restore upload with
 * per-chunk SHA-256, plan → validate → apply, polling to a terminal state, and
 * re-attaching to a job already running on the device. What changes is only
 * the output: instead of writing into the classic modal, every change is
 * reported to one `onChange` listener, so the new storage page can draw it.
 *
 * The chunk, header and request-ID helpers live in `config/` next to the
 * checksum code they share.
 */

import { request, t } from './data';
import { createMaintenanceRequestID } from './maintenance';
import { readArchiveHeader, triggerBrowserDownload, uploadBrowserChunks } from './transfers';

export const TERMINAL = new Set([
  'completed',
  'failed',
  'cancelled',
  'reboot_required',
  'rollback_failed',
]);
const RESTORE_SD_STRATEGY = 'allow_different';
/**
 * Restore states that wait for the archive bytes. After a refresh the chosen
 * file is gone, so a restore attached in one of these cannot go on; it can
 * only be cancelled and started again.
 */
const NEEDS_UPLOAD = [
  'created',
  'restore_ingest',
  'reading_manifest',
  'awaiting_plan',
  'uploading_and_staging',
];
/**
 * How long an attached restore must sit in `validating` before the page sends
 * /validate itself; a validation the previous page started finishes well
 * within this, ms.
 */
const RESUME_VALIDATE_AFTER_MS = 8000;
/** Once restore reaches these, the device commits regardless; cancelling is refused. */
const UNCANCELLABLE = ['committing', 'post_processing', 'resuming_services', 'rolling_back'];

const delay = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

/** `1536` → `1.5 KB`. */
export function formatBytes(value: unknown) {
  const bytes = Number(value || 0);
  if (!bytes) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB'];
  let index = 0;
  let amount = bytes;
  while (amount >= 1024 && index < units.length - 1) {
    amount /= 1024;
    index += 1;
  }
  return `${amount.toFixed(index === 0 ? 0 : 1)} ${units[index]}`;
}

/** A translated message for a backup error code, falling back to its text. */
export function errorText(caught: unknown) {
  const error = apiError(caught);
  if (!error) return '';
  const code = error.error || error.code || error.message;
  const known = t(`backup.error.${code}`, { defaultValue: '' });
  return known || error.message || String(code || 'error');
}

function phaseText(payload: BackupJob) {
  const phase = payload.phase || payload.state || '';
  return (
    t(`backup.phase.${phase}`, { defaultValue: '' }) ||
    t(`backup.component.${phase}`, { defaultValue: '' }) ||
    phase
  );
}

/**
 * @param {(state: object) => void} onChange - receives
 *   `{kind, running, stalled, phase, done, total, message, error, finished, rebootRequired}`;
 *   `stalled` marks an attached restore that lost its file and can only be cancelled.
 */
export function createBackupEngine(onChange: (state: BackupView) => void) {
  let session: Session | null = null;
  let job: BackupJob | null = null;
  let file: File | null = null;
  let header: ArchiveHeader | null = null;
  let identityConfirmed = false;
  let view: BackupView = {
    kind: null,
    running: false,
    stalled: false,
    phase: '',
    done: 0,
    total: 0,
    message: '',
    error: false,
    finished: false,
    rebootRequired: false,
  };

  // Leaving the page mid-job orphans a restore upload, so warn first, as
  // the classic page did.
  const onBeforeUnload = (event: BeforeUnloadEvent) => {
    event.preventDefault();
    event.returnValue = '';
  };
  let guarding = false;
  const emit = (patch: Partial<BackupView>) => {
    view = { ...view, ...patch };
    if (view.running !== guarding) {
      guarding = view.running;
      if (guarding) window.addEventListener('beforeunload', onBeforeUnload);
      else window.removeEventListener('beforeunload', onBeforeUnload);
    }
    onChange(view);
  };

  async function openSession() {
    if (session && new Date(session.expires_at).getTime() > Date.now() + 30000) return session;
    session = await request<Session>('/api/maintenance/sessions', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'X-Aico-Client': 'config-web/2.0' },
    });
    return session;
  }

  async function maintenance<T = BackupJob>(url: string, options: RequestInit = {}): Promise<T> {
    const id = createMaintenanceRequestID();
    for (let attempt = 0; attempt < 2; attempt += 1) {
      const current = await openSession();
      const headers = new Headers(options.headers || {});
      headers.set('X-Aico-Request-ID', id);
      headers.set('X-Aico-CSRF-Token', current.csrf_token);
      try {
        return await request<T>(url, { ...options, headers, credentials: 'same-origin' });
      } catch (caught) {
        const error = apiError(caught);
        if (
          attempt === 0 &&
          error.status === 401 &&
          error.error === 'maintenance_session_required'
        ) {
          session = null;
          continue;
        }
        throw error;
      }
    }
    throw new Error('maintenance_session_required');
  }

  const jobRequest = (url: string, options: RequestInit = {}): Promise<BackupJob> =>
    options.method && options.method !== 'GET'
      ? maintenance(url, options)
      : request<BackupJob>(url, { ...options, credentials: 'same-origin' });
  const jobPath = (kind: string | null, id: string, suffix = '') =>
    `/api/${kind}/jobs/${encodeURIComponent(id)}${suffix}`;

  function progress(payload: BackupJob) {
    const state = payload.state || '';
    if (view.kind === 'backup') {
      const total = Number(payload.estimated_bytes || 0);
      const done = Number(payload.bytes_read || 0);
      emit(
        state === 'streaming' && total > 0
          ? { phase: phaseText(payload), done, total }
          : { phase: phaseText(payload), done: 0, total: 0 },
      );
    } else {
      const total = Number(payload.archive_size || (file && file.size) || 0);
      const done = Number(payload.received_bytes || 0);
      const uploading =
        ['restore_ingest', 'reading_manifest', 'uploading_and_staging'].includes(state) &&
        total > 0;
      emit(
        uploading
          ? { phase: phaseText(payload), done, total }
          : { phase: phaseText(payload), done: 0, total: 0 },
      );
    }
  }

  function finish(payload: BackupJob) {
    job = { ...job, ...payload };
    const error = payload.error ? errorText(payload.error) : '';
    const restore = view.kind === 'restore';
    const messages: Record<string, string> = {
      completed: t(restore ? 'backup.restore_completed' : 'backup.create_completed'),
      reboot_required: t('backup.restore_reboot_required'),
      cancelled: t('backup.cancelled'),
      rollback_failed: t('backup.rollback_failed', { error }),
    };
    const failed = !['completed', 'reboot_required', 'cancelled'].includes(payload.state);
    const message =
      messages[payload.state] ||
      t(restore ? 'backup.restore_failed' : 'backup.create_failed', { error });
    const warnings = (payload.warnings || []).join('\n');
    emit({
      running: false,
      stalled: false,
      finished: true,
      error: failed,
      rebootRequired: payload.state === 'reboot_required',
      message: warnings ? `${message}\n${warnings}` : message,
      phase: '',
      done: 0,
      total: 0,
    });
  }

  function fail(error: unknown) {
    const restore = view.kind === 'restore';
    emit({
      running: false,
      stalled: false,
      finished: true,
      error: true,
      phase: '',
      done: 0,
      total: 0,
      message: t(restore ? 'backup.restore_failed' : 'backup.create_failed', {
        error: errorText(error),
      }),
    });
  }

  async function poll(kind: string, jobId: string) {
    for (let attempt = 0; attempt < 4 * 60 * 60; attempt += 1) {
      if (!job || job.job_id !== jobId) return null;
      let payload;
      try {
        payload = await jobRequest(jobPath(kind, jobId));
      } catch (caught) {
        const error = apiError(caught);
        if (error.status === 404) throw error;
        await delay(2000);
        continue;
      }
      job = { ...job, ...payload };
      progress(payload);
      if (TERMINAL.has(payload.state)) {
        finish(payload);
        return payload;
      }
      await delay(1000);
    }
    throw new Error(t('backup.error.timeout'));
  }

  /** What a backup would include; also proves the session is allowed (USB). */
  async function capabilities() {
    await openSession();
    return maintenance<Record<string, unknown>>('/api/backup/capabilities');
  }

  async function createBackup() {
    emit({
      kind: 'backup',
      running: true,
      finished: false,
      error: false,
      message: '',
      phase: t('backup.phase.ready'),
    });
    try {
      const response = await maintenance('/api/backup/jobs', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          format_version: 1,
          mode: 'same_device',
          protection: { mode: 'none' },
        }),
      });
      job = { kind: 'backup', ...response };
      // The device starts streaming only once the archive URL is requested.
      if (!response.archive_url) throw new Error('archive_url_missing');
      triggerBrowserDownload(response.archive_url, response.suggested_filename);
      await poll('backup', response.job_id);
    } catch (caught) {
      const error = apiError(caught);
      fail(error);
    }
  }

  /** Read and check a chosen archive; returns its creation time, if any. */
  async function chooseRestore(chosen: File) {
    file = chosen;
    header = await readArchiveHeader(chosen);
    if (header.protection?.algorithm !== 'sha256-chunked') {
      throw Object.assign(new Error(t('backup.error.encrypted_archive_unsupported')), {
        error: 'encrypted_archive_unsupported',
      });
    }
    return header.created_at ? new Date(header.created_at).toLocaleString() : '';
  }

  async function submitPlan(payload: BackupJob) {
    const manifest =
      payload.manifest || (await jobRequest(jobPath('restore', payload.job_id))).manifest;
    if (!manifest || !Array.isArray(manifest.components)) throw new Error('manifest_invalid');
    const response = await maintenance(jobPath('restore', payload.job_id, '/plan'), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        components: manifest.components.map((item) => item.id),
        sd_strategy: RESTORE_SD_STRATEGY,
        confirm_conflicts: true,
        confirm_identity: identityConfirmed,
      }),
    });
    job = { ...job, ...response };
  }

  async function validateAndApply(jobId: string) {
    emit({ phase: t('backup.phase.validating'), done: 0, total: 0 });
    const validated = await maintenance(jobPath('restore', jobId, '/validate'), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
    });
    job = { ...job, ...validated };
    await applyRestore(jobId, validated.plan_digest);
  }

  async function applyRestore(jobId: string, planDigest?: string) {
    emit({ phase: t('backup.phase.committing') });
    let applied;
    try {
      applied = await maintenance(jobPath('restore', jobId, '/apply'), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ plan_digest: planDigest, confirm: 'RESTORE' }),
      });
    } catch (caught) {
      const error = apiError(caught);
      // A dropped apply response does not stop the commit on the device; only
      // a definite rejection (bad digest, not prepared) is a real failure.
      if (error.status && error.status < 500 && error.error !== 'restore_committing') throw error;
      await poll('restore', jobId);
      return;
    }
    job = { ...job, ...applied };
    if (TERMINAL.has(applied.state)) finish(applied);
    else await poll('restore', jobId);
  }

  async function startRestore({ confirmIdentity }: { confirmIdentity: boolean }) {
    if (!file || !header) return;
    identityConfirmed = Boolean(confirmIdentity);
    emit({
      kind: 'restore',
      running: true,
      finished: false,
      error: false,
      message: '',
      phase: t('backup.phase.ready'),
    });
    try {
      const created = await maintenance('/api/restore/jobs', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          format_version: 1,
          archive_size: file.size,
          public_header: header,
          protection: { mode: 'none' },
        }),
      });
      job = { kind: 'restore', ...created, archive_size: file.size };
      emit({ phase: t('backup.phase.restore_ingest'), done: 0, total: file.size });
      await uploadBrowserChunks(
        file,
        created.chunk_size,
        (index, chunk, hash) =>
          jobRequest(jobPath('restore', created.job_id, `/chunks/${index}`), {
            method: 'PUT',
            headers: { 'Content-Type': 'application/octet-stream', 'X-Aico-Chunk-SHA256': hash },
            body: chunk,
          }),
        async (response, offset, total) => {
          job = { ...job, ...response };
          if (response.error)
            throw Object.assign(new Error(response.error.message || response.error.code), {
              error: response.error.code,
            });
          progress({ ...response, received_bytes: offset, archive_size: total });
          if (response.state === 'awaiting_plan') await submitPlan(response);
        },
      );
      await validateAndApply(created.job_id);
    } catch (caught) {
      const error = apiError(caught);
      if (job?.job_id && !UNCANCELLABLE.includes(job.state) && !TERMINAL.has(job.state)) {
        try {
          await maintenance(jobPath('restore', job.job_id), { method: 'DELETE' });
        } catch {
          // Already finished on the device.
        }
      }
      fail(error);
    }
  }

  /** Cancel the running job. A restore past its commit point cannot be. */
  async function cancel() {
    if (!job?.job_id || TERMINAL.has(job.state)) return false;
    if (view.kind === 'restore' && UNCANCELLABLE.includes(job.state)) {
      emit({ message: t('backup.restore_cannot_cancel'), error: true });
      return false;
    }
    const payload = await maintenance(jobPath(view.kind, job.job_id), { method: 'DELETE' });
    job = { ...job, ...payload };
    if (TERMINAL.has(payload.state)) finish(payload);
    return true;
  }

  /** Re-bind to a job already running on the device (a refresh, a second tab). */
  async function attach() {
    try {
      await openSession();
    } catch {
      // Another client may hold the session; status still reads without one.
    }
    const current = await request<{
      job_id?: string;
      operation?: string;
      phase?: string;
      job?: BackupJob;
    }>('/api/maintenance/current', { credentials: 'same-origin' }).catch(() => null);
    if (!current || !current.job_id || (job && job.job_id === current.job_id)) return false;
    const kind = current.operation === 'restore' ? 'restore' : 'backup';
    job = { kind, job_id: current.job_id, state: current.phase || '', ...(current.job || {}) };
    emit({
      kind,
      running: true,
      finished: false,
      error: false,
      message: t(kind === 'restore' ? 'backup.restore_started' : 'backup.create_started'),
    });
    const state = job.state || current.phase || '';
    progress(current.job || { job_id: current.job_id, state });
    if (kind === 'restore' && NEEDS_UPLOAD.includes(state)) {
      // Nothing on this page can send the rest of the archive. Say so and
      // leave Cancel as the way out, instead of showing progress that never
      // moves until the device's watchdog gives up.
      emit({
        phase: '',
        done: 0,
        total: 0,
        stalled: true,
        message: t('backup.restore_interrupted'),
      });
      poll(kind, current.job_id).catch((error) => fail(error));
      return true;
    }
    // The upload finished before the refresh; the device is waiting for the
    // same requests the uninterrupted flow would have sent next. The user
    // confirmed the restore before it started, so it simply goes on.
    if (kind === 'restore' && (state === 'validating' || state === 'prepared')) {
      resumeRestore(current.job_id).catch((error) => fail(error));
    } else {
      poll(kind, current.job_id).catch((error) => fail(error));
    }
    return true;
  }

  /**
   * Carry an attached, fully uploaded restore through validate and apply.
   *
   * `validating` means both "waiting for /validate" and "/validate running",
   * and the server does not serialise a second call; the request the old page
   * sent may still be in flight. So the job is watched first, and /validate is
   * sent only once it has sat in `validating` long enough that nobody is
   * running it. Any failure is reconciled against the job's real state before
   * it is reported, so a lost response does not abandon a job the device is
   * still holding.
   */
  async function resumeRestore(jobId: string) {
    let validateSent = false;
    const quietSince = Date.now();
    try {
      for (;;) {
        if (!job || job.job_id !== jobId) return;
        const payload = await jobRequest(jobPath('restore', jobId));
        job = { ...job, ...payload };
        if (payload.state === 'prepared' && payload.plan_digest) {
          await applyRestore(jobId, payload.plan_digest);
          return;
        }
        if (payload.state !== 'validating') {
          if (TERMINAL.has(payload.state)) finish(payload);
          else await poll('restore', jobId);
          return;
        }
        if (!validateSent && Date.now() - quietSince >= RESUME_VALIDATE_AFTER_MS) {
          validateSent = true;
          emit({ phase: t('backup.phase.validating'), done: 0, total: 0 });
          const validated = await maintenance(jobPath('restore', jobId, '/validate'), {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: '{}',
          });
          job = { ...job, ...validated };
          continue;
        }
        await delay(1000);
      }
    } catch (caught) {
      const error = apiError(caught);
      await reconcileRestore(jobId, error);
    }
  }

  /** After a failed resume step: continue, report the device's outcome, or cancel. */
  async function reconcileRestore(jobId: string, original: unknown) {
    let payload;
    try {
      payload = await jobRequest(jobPath('restore', jobId));
    } catch {
      fail(original);
      return;
    }
    job = { ...job, ...payload };
    if (TERMINAL.has(payload.state)) {
      finish(payload);
      return;
    }
    if (payload.state === 'prepared' && payload.plan_digest) {
      try {
        await applyRestore(jobId, payload.plan_digest);
        return;
      } catch {
        // Fall through to the cancel below with the first error.
      }
    } else if (UNCANCELLABLE.includes(payload.state)) {
      // Committing on the device already; it finishes on its own.
      await poll('restore', jobId);
      return;
    }
    try {
      await maintenance(jobPath('restore', jobId), { method: 'DELETE' });
    } catch {
      // Already finished on the device.
    }
    fail(original);
  }

  return {
    capabilities,
    createBackup,
    chooseRestore,
    startRestore,
    cancel,
    attach,
    fileName: () => (file ? file.name : ''),
  };
}
