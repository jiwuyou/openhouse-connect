export const MAX_IMAGE_ATTACHMENTS = 4;
export const MAX_IMAGE_BYTES = 5 * 1024 * 1024;

export const ALLOWED_IMAGE_MIME_TYPES = [
  'image/png',
  'image/jpeg',
  'image/webp',
  'image/gif',
] as const;

export type ImageSource = 'upload' | 'agent' | 'history';

export interface WebImageAttachment {
  id: string;
  mimeType: string;
  data?: string;
  url?: string;
  fileName?: string;
  size?: number;
  source?: ImageSource;
}

export interface BridgeImagePayload {
  mime_type: string;
  data: string;
  file_name?: string;
}

export interface ImageReadResult {
  attachments: WebImageAttachment[];
  errors: string[];
}

type RawImageAttachment = {
  id?: string;
  mimeType?: string;
  mime_type?: string;
  contentType?: string;
  content_type?: string;
  type?: string;
  data?: string;
  base64?: string;
  content?: string;
  url?: string;
  src?: string;
  imageUrl?: string;
  image_url?: string;
  fileName?: string;
  file_name?: string;
  name?: string;
  size?: number;
};

type SafeImageUrl =
  | { kind: 'url'; url: string }
  | { kind: 'data'; mimeType: string; data: string };

export function isAllowedImageType(mimeType: string) {
  return ALLOWED_IMAGE_MIME_TYPES.includes(normalizeMimeType(mimeType) as typeof ALLOWED_IMAGE_MIME_TYPES[number]);
}

export function bytesToLabel(size?: number) {
  if (size === undefined) return '';
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(1)} KB`;
  return `${(size / (1024 * 1024)).toFixed(1)} MB`;
}

export function stripDataUrlPrefix(data: string) {
  const comma = data.indexOf(',');
  if (data.startsWith('data:') && comma >= 0) {
    return data.slice(comma + 1);
  }
  return data;
}

export function imageAttachmentSrc(image: WebImageAttachment) {
  if (image.url) {
    const safeUrl = normalizeImageUrl(image.url);
    if (safeUrl?.kind === 'url') return safeUrl.url;
    if (safeUrl?.kind === 'data') return `data:${safeUrl.mimeType};base64,${safeUrl.data}`;
  }
  const safeData = normalizeImageData(image.data, image.mimeType);
  if (!safeData) return '';
  return `data:${safeData.mimeType};base64,${safeData.data}`;
}

export function toBridgeImagePayload(images: WebImageAttachment[]): BridgeImagePayload[] {
  return images
    .filter((img) => Boolean(img.data))
    .map((img) => ({
      mime_type: img.mimeType || 'image/png',
      data: stripDataUrlPrefix(img.data || ''),
      file_name: img.fileName || undefined,
    }));
}

export function normalizeImageAttachments(raw: unknown, source: ImageSource = 'history'): WebImageAttachment[] {
  if (!Array.isArray(raw)) return [];
  return raw
    .map((item, index) => normalizeImageAttachment(item as RawImageAttachment, index, source))
    .filter((item): item is WebImageAttachment => Boolean(item));
}

export function normalizeBridgeImageMessage(raw: unknown, source: ImageSource = 'agent'): WebImageAttachment[] {
  if (!raw || typeof raw !== 'object') return [];
  const msg = raw as { image?: unknown; images?: unknown };
  const items = Array.isArray(msg.images)
    ? msg.images
    : msg.image
      ? [msg.image]
      : [];
  return normalizeImageAttachments(items, source);
}

export async function readImageFiles(files: FileList | File[], currentCount = 0): Promise<ImageReadResult> {
  const selected = Array.from(files);
  const slots = Math.max(0, MAX_IMAGE_ATTACHMENTS - currentCount);
  const errors: string[] = [];

  if (slots <= 0) {
    return { attachments: [], errors: ['imageLimit'] };
  }
  if (selected.length > slots) {
    errors.push('imageLimit');
  }

  const valid = selected.slice(0, slots).filter((file) => {
    if (!isAllowedImageType(file.type)) {
      errors.push('imageUnsupported');
      return false;
    }
    if (file.size > MAX_IMAGE_BYTES) {
      errors.push('imageTooLarge');
      return false;
    }
    return true;
  });

  const settled = await Promise.allSettled(valid.map(readImageFile));
  const attachments: WebImageAttachment[] = [];
  for (const item of settled) {
    if (item.status === 'fulfilled') {
      attachments.push(item.value);
    } else {
      errors.push('imageReadFailed');
    }
  }

  return { attachments, errors: dedupe(errors) };
}

export function downloadImageAttachment(image: WebImageAttachment) {
  const fileName = image.fileName || `image-${image.id || Date.now()}.${extensionForMime(image.mimeType)}`;
  const link = document.createElement('a');
  link.download = fileName;

  if (image.data) {
    const safeData = normalizeImageData(image.data, image.mimeType);
    if (!safeData) return;
    const blob = base64ToBlob(safeData.data, safeData.mimeType);
    const objectUrl = URL.createObjectURL(blob);
    link.href = objectUrl;
    document.body.appendChild(link);
    link.click();
    link.remove();
    URL.revokeObjectURL(objectUrl);
    return;
  }

  const src = imageAttachmentSrc(image);
  if (!src) return;
  link.href = src;
  document.body.appendChild(link);
  link.click();
  link.remove();
}

function normalizeImageAttachment(item: RawImageAttachment, index: number, source: ImageSource): WebImageAttachment | null {
  if (!item || typeof item !== 'object') return null;
  const rawData = item.data || item.base64 || item.content || '';
  const explicitMimeType = item.mimeType || item.mime_type || item.contentType || item.content_type || item.type || '';
  const safeData = normalizeImageData(rawData ? String(rawData) : undefined, explicitMimeType);
  const safeUrl = normalizeImageUrl(item.url || item.src || item.imageUrl || item.image_url || undefined);
  if (!safeData && !safeUrl) return null;

  const normalizedExplicitMime = normalizeMimeType(explicitMimeType);
  const mimeType = safeData?.mimeType
    || (safeUrl?.kind === 'data' ? safeUrl.mimeType : isAllowedImageType(normalizedExplicitMime) ? normalizedExplicitMime : 'image/png');
  const fileName = item.fileName || item.file_name || item.name || undefined;
  const data = safeData?.data || (safeUrl?.kind === 'data' ? safeUrl.data : undefined);
  const url = safeUrl?.kind === 'url' ? safeUrl.url : undefined;
  const size = typeof item.size === 'number' ? item.size : data ? estimateBase64Size(data) : undefined;

  return {
    id: item.id || `${source}-${index}-${fileName || 'image'}`,
    mimeType,
    data,
    url,
    fileName,
    size,
    source,
  };
}

function readImageFile(file: File): Promise<WebImageAttachment> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onerror = () => reject(reader.error || new Error('read failed'));
    reader.onload = () => {
      const result = typeof reader.result === 'string' ? reader.result : '';
      resolve({
        id: crypto.randomUUID?.() || `upload-${Date.now()}-${Math.random().toString(16).slice(2)}`,
        mimeType: file.type || mimeFromDataUrl(result) || 'image/png',
        data: stripDataUrlPrefix(result),
        fileName: file.name,
        size: file.size,
        source: 'upload',
      });
    };
    reader.readAsDataURL(file);
  });
}

function mimeFromDataUrl(data: string) {
  const match = data.match(/^data:([^;,]+)[;,]/);
  return normalizeMimeType(match?.[1] || '');
}

function normalizeMimeType(mimeType?: string) {
  return (mimeType || '').trim().toLowerCase();
}

function normalizeImageData(data?: string, explicitMimeType?: string): { mimeType: string; data: string } | null {
  const raw = (data || '').trim();
  if (!raw) return null;

  if (raw.startsWith('data:')) {
    const parsed = parseSafeDataImageUrl(raw);
    if (!parsed || parsed.kind !== 'data') return null;
    return { mimeType: parsed.mimeType, data: parsed.data };
  }

  const mimeType = normalizeMimeType(explicitMimeType) || 'image/png';
  if (!isAllowedImageType(mimeType) || !looksLikeBase64(raw)) return null;
  return { mimeType, data: stripBase64Whitespace(raw) };
}

function normalizeImageUrl(url?: string): SafeImageUrl | null {
  const raw = (url || '').trim();
  if (!raw) return null;
  if (raw.startsWith('data:')) return parseSafeDataImageUrl(raw);

  try {
    const parsed = new URL(raw);
    if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') return null;
    return { kind: 'url', url: parsed.href };
  } catch {
    return null;
  }
}

function parseSafeDataImageUrl(url: string): SafeImageUrl | null {
  const match = url.match(/^data:(image\/(?:png|jpeg|webp|gif));base64,([a-z0-9+/=\s]+)$/i);
  if (!match) return null;
  const mimeType = normalizeMimeType(match[1]);
  const data = stripBase64Whitespace(match[2]);
  if (!isAllowedImageType(mimeType) || !looksLikeBase64(data)) return null;
  return { kind: 'data', mimeType, data };
}

function looksLikeBase64(data: string) {
  const stripped = stripBase64Whitespace(data);
  return stripped.length > 0 && stripped.length % 4 === 0 && /^[A-Za-z0-9+/]+={0,2}$/.test(stripped);
}

function stripBase64Whitespace(data: string) {
  return data.replace(/\s+/g, '');
}

function estimateBase64Size(data: string) {
  const normalized = stripDataUrlPrefix(data);
  const padding = normalized.endsWith('==') ? 2 : normalized.endsWith('=') ? 1 : 0;
  return Math.max(0, Math.floor((normalized.length * 3) / 4) - padding);
}

function extensionForMime(mimeType: string) {
  switch (mimeType) {
    case 'image/jpeg':
      return 'jpg';
    case 'image/webp':
      return 'webp';
    case 'image/gif':
      return 'gif';
    case 'image/png':
    default:
      return 'png';
  }
}

function base64ToBlob(data: string, mimeType: string) {
  const binary = atob(data);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) {
    bytes[i] = binary.charCodeAt(i);
  }
  return new Blob([bytes], { type: mimeType });
}

function dedupe(values: string[]) {
  return Array.from(new Set(values));
}
