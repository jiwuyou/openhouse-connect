import { memo, useEffect, useState, useRef, useCallback, useMemo, Fragment } from 'react';
import { useTranslation } from 'react-i18next';
import { useParams, Link, useSearchParams } from 'react-router-dom';
import {
  ArrowLeft, Send, User, Bot, Circle, WifiOff,
  Copy, Check, FileText, Image as ImageIcon, Loader2,
  Slash, ChevronDown,
} from 'lucide-react';
import { Button } from '@/components/ui';
import { listSessions, getSession, createSession, updateSession, sendMessage, type Session, type SessionDetail, type RunEvent } from '@/api/sessions';
import { listProjectFrontendSlots, type FrontendSlot } from '@/api/bridge';
import { getGlobalSettings, type RunTraceMode } from '@/api/settings';
import CommandPalette, { type SlashCommand, slashCommands } from './CommandPalette';
import SessionDrawer from './SessionDrawer';
import CommandResultPanel, { type CommandResult } from './CommandResultPanel';
import { ImageAttachmentGrid, ImageAttachmentPreview } from '@/components/ImageAttachments';
import Markdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import rehypeHighlight from 'rehype-highlight';
import { cn } from '@/lib/utils';
import { RunTrace } from '@/components/RunTrace';
import api from '@/api/client';
import {
  ALLOWED_IMAGE_MIME_TYPES,
  MAX_IMAGE_ATTACHMENTS,
  normalizeImageAttachments,
  readImageFiles,
  toBridgeImagePayload,
  type WebImageAttachment,
} from '@/lib/attachments';
import {
  frontendSlotOptionsFrom,
  getInitialWebBridgePlatform,
  isLegacyWebBridgeSessionKey,
  legacyWebSessionKeys,
  normalizeWebBridgePlatform,
  persistWebBridgePlatform,
  platformFromSessionKey,
  selectedFrontendSlot,
  webSessionKey,
} from '@/lib/webPlatform';

// ── Markdown renderers ───────────────────────────────────────

function CopyButton({ code }: { code: string }) {
  const [copied, setCopied] = useState(false);
  const handleCopy = () => {
    navigator.clipboard.writeText(code);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };
  return (
    <button
      onClick={handleCopy}
      className="absolute top-2 right-2 p-1.5 rounded-md bg-gray-200/80 dark:bg-gray-700/80 hover:bg-gray-300 dark:hover:bg-gray-600 text-gray-500 dark:text-gray-400 opacity-0 group-hover:opacity-100 transition-opacity z-10"
    >
      {copied ? <Check size={12} /> : <Copy size={12} />}
    </button>
  );
}

function PreBlock({ children, ...props }: React.HTMLAttributes<HTMLPreElement>) {
  const codeEl = (children as any)?.props;
  const lang = codeEl?.className?.replace(/^language-/, '') || '';
  const code = typeof codeEl?.children === 'string' ? codeEl.children.replace(/\n$/, '') : '';
  return (
    <div className="not-prose relative group my-4">
      {lang && (
        <div className="absolute top-0 left-0 px-2.5 py-1 text-[10px] font-medium uppercase tracking-wider text-gray-400 dark:text-gray-500 bg-gray-100 dark:bg-gray-800 rounded-tl-lg rounded-br-lg border-b border-r border-gray-200 dark:border-gray-700 font-mono">
          {lang}
        </div>
      )}
      <CopyButton code={code} />
      <pre className="overflow-x-auto rounded-lg bg-[#fafafa] dark:bg-[#0d1117] border border-gray-200 dark:border-gray-700/60 p-3 sm:p-4 pt-8 text-xs sm:text-[13px] leading-[1.6] font-mono" {...props}>
        {children}
      </pre>
    </div>
  );
}

function InlineCode({ children, className, ...props }: React.HTMLAttributes<HTMLElement>) {
  if (className) return <code className={className} {...props}>{children}</code>;
  return (
    <code className="px-1.5 py-0.5 rounded-md bg-gray-100 dark:bg-gray-800 text-pink-600 dark:text-pink-400 text-[0.875em] font-mono border border-gray-200/60 dark:border-gray-700/40" {...props}>
      {children}
    </code>
  );
}

const RenderMarkdown = memo(function RenderMarkdown({ content }: { content: string }) {
  return (
    <div className={cn(
      'prose prose-sm sm:prose-base max-w-none dark:prose-invert break-words',
      'prose-headings:font-semibold prose-headings:tracking-tight',
      'prose-h1:text-xl prose-h1:mt-5 prose-h1:mb-3 prose-h1:pb-1.5 prose-h1:border-b prose-h1:border-gray-200 dark:prose-h1:border-gray-700',
      'prose-h2:text-lg prose-h2:mt-5 prose-h2:mb-2',
      'prose-h3:text-base prose-h3:mt-4 prose-h3:mb-2',
      'prose-p:my-2.5 prose-p:leading-relaxed',
      'prose-li:my-0.5', 'prose-ul:my-2 prose-ol:my-2',
      'prose-a:text-accent prose-a:no-underline hover:prose-a:underline',
      'prose-strong:text-gray-900 dark:prose-strong:text-white prose-strong:font-semibold',
      'prose-blockquote:border-l-[3px] prose-blockquote:border-accent/40 prose-blockquote:bg-accent/[0.03] prose-blockquote:rounded-r-lg prose-blockquote:py-0.5 prose-blockquote:px-4 prose-blockquote:my-3 prose-blockquote:not-italic prose-blockquote:text-gray-600 dark:prose-blockquote:text-gray-300',
      'prose-hr:my-5 prose-hr:border-gray-200 dark:prose-hr:border-gray-700',
      'prose-table:text-sm prose-th:bg-gray-50 dark:prose-th:bg-gray-800 prose-th:px-3 prose-th:py-2 prose-td:px-3 prose-td:py-2',
      'prose-img:rounded-lg prose-img:shadow-sm',
    )}>
      <Markdown remarkPlugins={[remarkGfm]} rehypePlugins={[rehypeHighlight]} components={{ pre: PreBlock as any, code: InlineCode as any }}>
        {content}
      </Markdown>
    </div>
  );
});

// ── Chat message types ───────────────────────────────────────

interface ChatMsg {
  id: string;
  seq?: number;
  runId?: string;
  userMessageId?: string;
  role: 'user' | 'assistant' | 'system';
  content: string;
  format?: 'text' | 'markdown' | 'card' | 'buttons' | 'image' | 'file';
  card?: any;
  buttons?: { text: string; data: string }[][];
  imageUrl?: string;
  fileName?: string;
  fileSize?: number;
  images?: WebImageAttachment[];
  files?: WebFileAttachment[];
  streaming?: boolean;
  timestamp?: string;
}

// ── Helpers ──────────────────────────────────────────────────

function parseListItemText(text: string): { cmd: string; desc: string } {
  const m = text.match(/^\*\*(.+?)\*\*\s*(.*)/);
  if (m) return { cmd: m[1], desc: m[2] };
  const sp = text.indexOf(' ');
  if (sp > 0) return { cmd: text.slice(0, sp), desc: text.slice(sp + 1) };
  return { cmd: text, desc: '' };
}

function InlineMd({ text }: { text: string }) {
  const parts = text.split(/(\*\*[^*]+\*\*)/g);
  return (
    <>
      {parts.map((p, i) =>
        p.startsWith('**') && p.endsWith('**')
          ? <strong key={i} className="font-semibold text-gray-900 dark:text-white">{p.slice(2, -2)}</strong>
          : <span key={i}>{p}</span>
      )}
    </>
  );
}

// ── Card renderer (flat, clean style for in-stream cards) ────

function CardBlock({ card, onAction }: { card: any; onAction: (v: string) => void }) {
  if (!card) return null;
  return (
    <div className="space-y-3">
      {card.header?.title && (
        <div className="text-sm font-semibold text-gray-900 dark:text-white">{card.header.title}</div>
      )}
      {card.elements?.map((el: any, i: number) => (
        <CardElement key={i} el={el} onAction={onAction} />
      ))}
    </div>
  );
}

function CardElement({ el, onAction }: { el: any; onAction: (v: string) => void }) {
  if (el.type === 'markdown') return <RenderMarkdown content={el.content} />;
  if (el.type === 'divider') return <div className="border-t border-gray-200/60 dark:border-gray-700/40" />;
  if (el.type === 'note') return <p className="text-[11px] text-gray-400 dark:text-gray-500">{el.text}</p>;
  if (el.type === 'actions') {
    return (
      <div className="flex flex-wrap gap-2">
        {el.buttons?.map((btn: any, j: number) => (
          <button key={j} onClick={() => onAction(btn.value)} className={cn(
            'px-3 py-1.5 rounded-lg text-xs font-medium transition-all duration-150',
            btn.btn_type === 'primary' ? 'bg-accent text-black hover:bg-accent-dim shadow-sm' :
            btn.btn_type === 'danger' ? 'bg-red-500/10 text-red-600 dark:text-red-400 hover:bg-red-500/20' :
            'bg-gray-100 dark:bg-gray-800 text-gray-600 dark:text-gray-300 hover:bg-gray-200 dark:hover:bg-gray-700',
          )}>
            {btn.text}
          </button>
        ))}
      </div>
    );
  }
  if (el.type === 'list_item') {
    const parsed = parseListItemText(el.text);
    const isCommand = parsed.cmd.startsWith('/');
    return (
      <button
        onClick={() => onAction(el.btn_value)}
        className="w-full flex items-center gap-3 py-2 text-left group"
      >
        {isCommand ? (
          <>
            <code className="shrink-0 w-20 text-xs font-mono font-medium text-accent">{parsed.cmd}</code>
            <span className="flex-1 text-sm text-gray-500 dark:text-gray-400 truncate">{parsed.desc}</span>
          </>
        ) : (
          <span className="flex-1 text-sm text-gray-700 dark:text-gray-300 truncate min-w-0">
            <InlineMd text={el.text} />
          </span>
        )}
        <span className={cn(
          'shrink-0 px-2 py-0.5 rounded-md text-[11px] font-medium transition-all',
          el.btn_type === 'primary'
            ? 'bg-accent/15 text-accent group-hover:bg-accent/25'
            : 'text-gray-400 dark:text-gray-500 bg-gray-100 dark:bg-gray-800 group-hover:bg-accent/15 group-hover:text-accent',
        )}>
          {el.btn_text}
        </span>
      </button>
    );
  }
  if (el.type === 'select') {
    return (
      <select
        defaultValue={el.init_value}
        onChange={(e) => onAction(e.target.value)}
        className="w-full px-3 py-2 text-sm rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800/80 text-gray-900 dark:text-white focus:outline-none focus:ring-2 focus:ring-accent/40"
      >
        {el.options?.map((opt: any, j: number) => (
          <option key={j} value={opt.value}>{opt.text}</option>
        ))}
      </select>
    );
  }
  return null;
}

function ButtonsBlock({ content, buttons, onAction }: { content: string; buttons: { text: string; data: string }[][]; onAction: (v: string) => void }) {
  return (
    <div className="space-y-3">
      <RenderMarkdown content={content} />
      {buttons.map((row, i) => (
        <div key={i} className="flex flex-wrap gap-2">
          {row.map((btn, j) => (
            <button key={j} onClick={() => onAction(btn.data)} className="px-3 py-1.5 rounded-lg text-xs font-medium bg-accent text-black hover:bg-accent-dim transition-colors">
              {btn.text}
            </button>
          ))}
        </div>
      ))}
    </div>
  );
}

function FileBlock({ name, size, url }: { name: string; size?: number; url?: string }) {
  const inner = (
    <div className="flex items-center gap-2 px-3 py-2 rounded-lg bg-gray-50 dark:bg-gray-800 border border-gray-200 dark:border-gray-700">
      <FileText size={16} className="text-gray-400 shrink-0" />
      <div className="min-w-0">
        <div className="text-sm font-medium text-gray-900 dark:text-white truncate">{name}</div>
        {size !== undefined && <div className="text-xs text-gray-400">{(size / 1024).toFixed(1)} KB</div>}
      </div>
    </div>
  );
  if (!url) return inner;
  return (
    <a href={url} target="_blank" rel="noreferrer" className="block hover:opacity-95 transition-opacity">
      {inner}
    </a>
  );
}

interface WebFileAttachment {
  id: string;
  url?: string;
  fileName?: string;
  size?: number;
  mimeType?: string;
}

function ImageBlock({ url }: { url: string }) {
  return <ImageAttachmentGrid images={[{ id: url, mimeType: 'image/png', url }]} />;
}

type RealtimeStatus = 'connecting' | 'connected' | 'disconnected' | 'error';

function StatusBadge({ status }: { status: RealtimeStatus }) {
  const { t } = useTranslation();
  if (status === 'connected') {
    return (
      <span className="flex items-center gap-1 text-[10px] text-emerald-600 dark:text-emerald-400 bg-emerald-50 dark:bg-emerald-900/20 px-1.5 py-0.5 rounded-full">
        <Circle size={5} className="fill-current" /> {t('sessions.bridgeConnected')}
      </span>
    );
  }
  if (status === 'connecting') {
    return (
      <span className="flex items-center gap-1 text-[10px] text-yellow-600 dark:text-yellow-400 bg-yellow-50 dark:bg-yellow-900/20 px-1.5 py-0.5 rounded-full">
        <Loader2 size={9} className="animate-spin" /> {t('sessions.bridgeConnecting')}
      </span>
    );
  }
  return (
    <span className="flex items-center gap-1 text-[10px] text-gray-400 bg-gray-100 dark:bg-gray-800 px-1.5 py-0.5 rounded-full">
      <WifiOff size={9} /> {t('sessions.bridgeDisconnected')}
    </span>
  );
}

const TypingIndicator = memo(function TypingIndicator() {
  return (
    <div className="flex gap-3 justify-start">
      <div className="w-8 h-8 rounded-lg bg-accent/10 flex items-center justify-center shrink-0 mt-1">
        <Bot size={16} className="text-accent" />
      </div>
      <div className="rounded-2xl px-5 py-3.5 text-sm bg-white dark:bg-gray-800/80 border border-gray-200 dark:border-gray-700/60 rounded-bl-md shadow-sm">
        <div className="flex gap-1.5">
          <span className="w-2 h-2 bg-gray-400 rounded-full animate-bounce" style={{ animationDelay: '0ms' }} />
          <span className="w-2 h-2 bg-gray-400 rounded-full animate-bounce" style={{ animationDelay: '150ms' }} />
          <span className="w-2 h-2 bg-gray-400 rounded-full animate-bounce" style={{ animationDelay: '300ms' }} />
        </div>
      </div>
    </div>
  );
});

const MessageBubble = memo(function MessageBubble({ msg, onAction }: { msg: ChatMsg; onAction: (value: string) => void }) {
  const { t } = useTranslation();
  const isUser = msg.role === 'user';
  const hasImages = Boolean(msg.images?.length);
  const hasFiles = Boolean(msg.files?.length);
  return (
    <div className={cn('flex gap-2 sm:gap-3 min-w-0', isUser ? 'justify-end' : 'justify-start')}>
      {!isUser && (
        <div className="w-7 h-7 sm:w-8 sm:h-8 rounded-lg bg-accent/10 flex items-center justify-center shrink-0 mt-1">
          <Bot size={14} className="text-accent sm:hidden" />
          <Bot size={16} className="text-accent hidden sm:block" />
        </div>
      )}
      <div className={cn(
        'rounded-2xl px-4 sm:px-5 py-3 sm:py-3.5 text-sm min-w-0 break-words',
        isUser
          ? 'max-w-[86%] sm:max-w-[70%] bg-accent text-black rounded-br-md'
          : 'max-w-[calc(100%-2.25rem)] sm:max-w-[85%] bg-white dark:bg-gray-800/80 border border-gray-200 dark:border-gray-700/60 text-gray-900 dark:text-gray-100 rounded-bl-md shadow-sm',
        msg.streaming && 'animate-pulse-subtle',
      )}>
        {msg.format === 'card' ? (
          <CardBlock card={msg.card} onAction={onAction} />
        ) : msg.format === 'buttons' && msg.buttons ? (
          <ButtonsBlock content={msg.content} buttons={msg.buttons} onAction={onAction} />
        ) : msg.format === 'image' && msg.imageUrl ? (
          <ImageBlock url={msg.imageUrl} />
        ) : msg.format === 'file' && msg.fileName ? (
          <FileBlock name={msg.fileName} size={msg.fileSize} />
        ) : isUser ? (
          msg.content ? (
          <div className="whitespace-pre-wrap">{msg.content}</div>
          ) : null
        ) : (
          msg.content ? <RenderMarkdown content={msg.content} /> : null
        )}
        {hasImages && (
          <ImageAttachmentGrid
            images={msg.images || []}
            className={msg.content || msg.format === 'image' || msg.format === 'file' ? 'mt-3' : ''}
            downloadLabel={t('chat.downloadImage')}
          />
        )}
        {hasFiles && (
          <div className={cn('space-y-2', msg.content || hasImages ? 'mt-3' : '')}>
            {(msg.files || []).map((f) => (
              <FileBlock key={f.id} name={f.fileName || 'file'} size={f.size} url={f.url} />
            ))}
          </div>
        )}
        {msg.streaming && (
          <span className="inline-block w-1.5 h-4 bg-accent/60 rounded-sm ml-0.5 animate-pulse" />
        )}
      </div>
      {isUser && (
        <div className="w-7 h-7 sm:w-8 sm:h-8 rounded-lg bg-gray-200 dark:bg-gray-700 flex items-center justify-center shrink-0 mt-1">
          <User size={14} className="text-gray-500 sm:hidden" />
          <User size={16} className="text-gray-500 hidden sm:block" />
        </div>
      )}
    </div>
  );
});

function ChatComposer({
  canSend,
  sending,
  placeholder,
  commandLabel,
  onSend,
  onCommandSelect,
}: {
  canSend: boolean;
  sending: boolean;
  placeholder: string;
  commandLabel: string;
  onSend: (content: string, images: WebImageAttachment[]) => boolean | Promise<boolean>;
  onCommandSelect: (cmd: SlashCommand) => void;
}) {
  const { t } = useTranslation();
  const [input, setInput] = useState('');
  const [images, setImages] = useState<WebImageAttachment[]>([]);
  const [imageError, setImageError] = useState('');
  const [cmdOpen, setCmdOpen] = useState(false);
  const cmdBtnRef = useRef<HTMLButtonElement>(null);
  const imageInputRef = useRef<HTMLInputElement>(null);
  const hasDraft = Boolean(input.trim()) || images.length > 0;

  const submit = useCallback(async () => {
    const content = input.trim();
    if (!hasDraft || !canSend || sending) return;
    if (await onSend(content, images)) {
      setInput('');
      setImages([]);
      setImageError('');
    }
  }, [canSend, hasDraft, images, input, onSend, sending]);

  const handleImagePick = useCallback(async (e: React.ChangeEvent<HTMLInputElement>) => {
    const inputEl = e.currentTarget;
    const files = inputEl.files;
    if (!files?.length) return;
    const result = await readImageFiles(files, images.length);
    if (result.attachments.length) {
      setImages(prev => [...prev, ...result.attachments].slice(0, MAX_IMAGE_ATTACHMENTS));
    }
    setImageError(result.errors.map((key) => t(`chat.${key}`)).filter(Boolean).join(' '));
    inputEl.value = '';
  }, [images.length, t]);

  const removeImage = useCallback((id: string) => {
    setImages(prev => prev.filter((image) => image.id !== id));
    setImageError('');
  }, []);

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      submit();
    }
    if (e.key === '/' && !input) {
      e.preventDefault();
      setCmdOpen(true);
    }
  };

  const handleCommandSelect = useCallback((cmd: SlashCommand) => {
    setCmdOpen(false);
    onCommandSelect(cmd);
  }, [onCommandSelect]);

  return (
    <div className="space-y-2">
      <ImageAttachmentPreview
        images={images}
        onRemove={removeImage}
        removeLabel={t('chat.removeImage')}
      />
      {imageError && <p className="text-xs text-red-600 dark:text-red-400">{imageError}</p>}
      <div className="relative flex items-end gap-1.5 sm:gap-2">
      <div className="relative">
        <button
          ref={cmdBtnRef}
          type="button"
          onClick={() => setCmdOpen((open) => !open)}
          className={cn(
            'p-3 min-h-11 min-w-11 rounded-xl transition-all duration-200',
            cmdOpen
              ? 'bg-accent/15 text-accent ring-1 ring-accent/30'
              : 'text-gray-400 hover:text-gray-600 dark:hover:text-gray-200 hover:bg-gray-100 dark:hover:bg-white/[0.06]',
          )}
          title={commandLabel}
        >
          <Slash size={18} />
        </button>
        <CommandPalette
          open={cmdOpen}
          onClose={() => setCmdOpen(false)}
          onSelect={handleCommandSelect}
          anchorRef={cmdBtnRef}
        />
      </div>

      <button
        type="button"
        onClick={() => imageInputRef.current?.click()}
        disabled={sending || images.length >= MAX_IMAGE_ATTACHMENTS}
        className="p-3 min-h-11 min-w-11 rounded-xl text-gray-400 hover:text-gray-600 dark:hover:text-gray-200 hover:bg-gray-100 dark:hover:bg-white/[0.06] transition-all duration-200 disabled:opacity-40"
        title={t('chat.attachImages')}
      >
        <ImageIcon size={18} />
      </button>
      <input
        ref={imageInputRef}
        type="file"
        accept={ALLOWED_IMAGE_MIME_TYPES.join(',')}
        multiple
        className="hidden"
        onChange={handleImagePick}
      />

      <div className="flex-1 relative">
        <input
          value={input}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={handleKeyDown}
          placeholder={placeholder}
          className="w-full px-4 py-3 text-base sm:text-sm rounded-xl border border-gray-300 dark:border-gray-700 bg-white dark:bg-gray-800 text-gray-900 dark:text-white focus:outline-none focus:ring-2 focus:ring-accent/50 focus:border-accent transition-colors placeholder:text-gray-400"
          disabled={sending}
        />
      </div>

      <button
        type="button"
        onClick={submit}
        disabled={sending || !hasDraft}
        className="p-3 min-h-11 min-w-11 rounded-xl bg-accent text-black hover:bg-accent-dim transition-colors disabled:opacity-50 flex items-center justify-center"
      >
        {sending ? <Loader2 size={18} className="animate-spin" /> : <Send size={18} />}
      </button>
      </div>
    </div>
  );
}

// ── Main component ───────────────────────────────────────────

function findBestLegacyWebSession(sessions: Session[], projectName: string, preferredPlatform: string) {
  const legacyKeys = new Set(legacyWebSessionKeys(projectName, preferredPlatform));
  return sessions
    .filter((s) => legacyKeys.has(s.session_key))
    .sort((a, b) => {
      const histDelta = (b.history_count || 0) - (a.history_count || 0);
      if (histDelta !== 0) return histDelta;
      return (b.updated_at || b.created_at || '').localeCompare(a.updated_at || a.created_at || '');
    })[0] || null;
}

function historyToMessages(history: SessionDetail['history'] | undefined): ChatMsg[] {
  const src = history || [];
  const copy = [...src];
  const hasSeq = copy.some((h: any) => typeof h?.seq === 'number');
  if (hasSeq) {
    copy.sort((a: any, b: any) => (a?.seq ?? 0) - (b?.seq ?? 0));
  }

  const seen = new Set<string>();
  const out: ChatMsg[] = [];
  copy.forEach((h: any, i: number) => {
    const id = h.id || (typeof h.seq === 'number' ? `seq-${h.seq}` : `hist-${i}`);
    if (seen.has(id)) return;
    seen.add(id);

    const images = normalizeImageAttachments(h.images, 'history');
    const files = normalizeFileAttachments(h.files);

    out.push({
      id,
      seq: typeof h.seq === 'number' ? h.seq : undefined,
      runId: h.run_id,
      userMessageId: h.user_message_id,
      role: h.role as 'user' | 'assistant',
      content: h.content || '',
      format: 'markdown',
      images,
      files,
      timestamp: h.timestamp || h.created_at,
    });
  });
  return out;
}

function normalizeRunEvents(events: RunEvent[] | undefined): RunEvent[] {
  const src = events || [];
  const copy = [...src];
  const hasSeq = copy.some((e: any) => typeof e?.seq === 'number');
  if (hasSeq) {
    copy.sort((a: any, b: any) => (a?.seq ?? 0) - (b?.seq ?? 0));
  }
  const seen = new Set<string>();
  return copy.filter((e) => {
    if (!e?.id) return true;
    if (seen.has(e.id)) return false;
    seen.add(e.id);
    return true;
  });
}

function mergeRunEvent(prev: RunEvent[], incoming: RunEvent): RunEvent[] {
  if (!incoming?.id) return prev;
  const byId = new Map<string, RunEvent>();
  for (const e of prev) {
    if (e?.id) byId.set(e.id, e);
  }
  const existing = byId.get(incoming.id);
  byId.set(incoming.id, existing ? { ...existing, ...incoming } : incoming);
  return normalizeRunEvents(Array.from(byId.values()));
}

function normalizeFileAttachments(raw: unknown): WebFileAttachment[] {
  if (!Array.isArray(raw)) return [];
  return raw
    .map((item: any, idx: number): WebFileAttachment | null => {
      if (!item || typeof item !== 'object') return null;
      const id = String(item.id || `file-${idx}`);
      const url = typeof item.url === 'string' ? item.url : undefined;
      const fileName = (item.file_name || item.fileName || item.name || item.file || '').toString() || undefined;
      const size = typeof item.size === 'number' ? item.size : undefined;
      const mimeType = (item.mime_type || item.mimeType || item.type || '').toString() || undefined;
      return { id, url, fileName, size, mimeType };
    })
    .filter((v): v is WebFileAttachment => Boolean(v));
}

function hasAssistantFinalAfterUser(messages: ChatMsg[], userIdx: number): boolean {
  for (let i = userIdx + 1; i < messages.length; i++) {
    const m = messages[i];
    if (m.role === 'user') return false;
    if (m.role === 'assistant' && !m.streaming) return true;
  }
  return false;
}

export default function ChatView() {
  const { t } = useTranslation();
  const { name: projectName } = useParams<{ name: string }>();
  const [searchParams] = useSearchParams();
  const requestedSessionId = searchParams.get('session') || '';

  // Session state
  const [sessions, setSessions] = useState<Session[]>([]);
  const [currentSession, setCurrentSession] = useState<SessionDetail | null>(null);
  const [messages, setMessages] = useState<ChatMsg[]>([]);
  const [runEvents, setRunEvents] = useState<RunEvent[]>([]);
  const [sending, setSending] = useState(false);
  const [loading, setLoading] = useState(true);
  const [typing, setTyping] = useState(false);
  const [rtStatus, setRtStatus] = useState<RealtimeStatus>('disconnected');
  const [webPlatform, setWebPlatform] = useState(() => getInitialWebBridgePlatform());
  const [frontendSlots, setFrontendSlots] = useState<FrontendSlot[]>([]);
  // Whether the user explicitly picked a session from the drawer
  const [userPickedSession, setUserPickedSession] = useState(false);

  // UI state
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [cmdResult, setCmdResult] = useState<CommandResult | null>(null);
  const [runTraceMode, setRunTraceMode] = useState<RunTraceMode>('auto');
  const [openRunTraces, setOpenRunTraces] = useState<Record<string, boolean>>({});

  const messagesEnd = useRef<HTMLDivElement>(null);
  const sessionKeyRef = useRef('');
  const sessionIdRef = useRef('');
  const knownMsgIdsRef = useRef<Set<string>>(new Set());
  // Track pending slash command so the next reply can be routed to the panel
  const pendingCmdRef = useRef<string | null>(null);
  // Mirrors cmdResult.command so card-action callbacks can route follow-ups back to the panel
  const cmdPanelRef = useRef<string | null>(null);

  // webnew is the stable conversation identity. Frontend slots are delivery routes.
  const defaultWebSessionKey = projectName ? webSessionKey(projectName) : '';
  const frontendSlotOptions = useMemo(() => frontendSlotOptionsFrom(frontendSlots), [frontendSlots]);
  const activeFrontendSlot = useMemo(() => selectedFrontendSlot(frontendSlotOptions, webPlatform), [frontendSlotOptions, webPlatform]);
  const activeFrontendSlotName = activeFrontendSlot.slot;
  const sessionKey = userPickedSession && currentSession?.session_key
    ? currentSession.session_key
    : defaultWebSessionKey;
  const currentSessionId = currentSession?.id || '';
  sessionKeyRef.current = sessionKey;
  sessionIdRef.current = currentSessionId;

  const applySessionDetail = useCallback((detail: SessionDetail | null) => {
    setCurrentSession(detail);
    const nextMsgs = detail ? historyToMessages(detail.history) : [];
    const nextRunEvents = detail ? normalizeRunEvents(detail.run_events) : [];
    setMessages(nextMsgs);
    setRunEvents(nextRunEvents);
    setOpenRunTraces({});

    const ids = new Set<string>();
    for (const m of nextMsgs) {
      ids.add(m.id);
    }
    knownMsgIdsRef.current = ids;
  }, []);

  // Load project sessions and auto-select latest
  const fetchData = useCallback(async () => {
    if (!projectName) return;
    setLoading(true);
    try {
      const [{ sessions: allSessions }, slots] = await Promise.all([
        listSessions(projectName),
        listProjectFrontendSlots(projectName),
      ]);
      setFrontendSlots(slots);
      const selectedSlot = selectedFrontendSlot(slots, webPlatform).slot;
      if (selectedSlot !== normalizeWebBridgePlatform(webPlatform)) {
        setWebPlatform(selectedSlot);
        persistWebBridgePlatform(selectedSlot);
      }
      const sorted = (allSessions || []).sort(
        (a, b) => (b.updated_at || b.created_at || '').localeCompare(a.updated_at || a.created_at || ''),
      );
      setSessions(sorted);

      const requestedSession = requestedSessionId
        ? sorted.find((s) => s.id === requestedSessionId)
        : null;
      if (requestedSession) {
        const detail = await getSession(projectName, requestedSession.id, 200);
        setUserPickedSession(true);
        applySessionDetail(detail);
        return;
      }

      const defaultSession = sorted.find((s) => s.session_key === defaultWebSessionKey);
      const legacySession = defaultSession ? null : findBestLegacyWebSession(sorted, projectName, webPlatform);
      const selectedSession = defaultSession || legacySession;
      if (selectedSession) {
        const detail = await getSession(projectName, selectedSession.id, 200);
        // Legacy web/bridge sessions do not have a backend alias yet, so keep using
        // their original key to avoid starting a fresh empty webnew agent session.
        setUserPickedSession(Boolean(legacySession));
        applySessionDetail(detail);
      } else {
        applySessionDetail(null);
      }
    } finally {
      setLoading(false);
    }
  }, [projectName, defaultWebSessionKey, requestedSessionId, webPlatform, applySessionDetail]);

  useEffect(() => { fetchData(); }, [fetchData]);

  // Webclient display preferences (best-effort).
  useEffect(() => {
    let alive = true;
    (async () => {
      try {
        const s = await getGlobalSettings();
        const mode = (s.webclient_display?.run_trace_mode || s.run_trace_mode || 'auto') as RunTraceMode;
        if (alive) setRunTraceMode(mode);
      } catch {
        // ignore
      }
    })();
    return () => { alive = false; };
  }, []);

  // Keep ref in sync with cmdResult so callbacks avoid stale closures
  useEffect(() => {
    cmdPanelRef.current = cmdResult?.command ?? null;
  }, [cmdResult]);

  // Switch to a different session (user explicitly chose from drawer)
  const switchToSession = useCallback(async (s: Session) => {
    if (!projectName) return;
    setDrawerOpen(false);
    setLoading(true);
    setUserPickedSession(true);
    try {
      const detail = await getSession(projectName, s.id, 200);
      if (isLegacyWebBridgeSessionKey(detail.session_key)) {
        const nextPlatform = normalizeWebBridgePlatform(platformFromSessionKey(detail.session_key));
        setWebPlatform(nextPlatform);
        persistWebBridgePlatform(nextPlatform);
      }
      applySessionDetail(detail);
    } finally {
      setLoading(false);
    }
  }, [projectName, applySessionDetail]);

  const handleWebPlatformChange = useCallback((value: string) => {
    const nextSlot = normalizeWebBridgePlatform(value);
    setWebPlatform(nextSlot);
    persistWebBridgePlatform(nextSlot);
    setUserPickedSession(false);
    applySessionDetail(null);
  }, [applySessionDetail]);

  // Realtime: subscribe to 9840 backend SSE (durable messages only).
  useEffect(() => {
    if (!projectName || !currentSessionId) {
      setRtStatus('disconnected');
      return;
    }

    const token = api.getToken();
    const base = `/api/projects/${encodeURIComponent(projectName)}/sessions/${encodeURIComponent(currentSessionId)}/events`;
    const url = token ? `${base}?token=${encodeURIComponent(token)}` : base;
    const es = new EventSource(url);
    setRtStatus('connecting');

    es.onopen = () => setRtStatus('connected');
    es.onerror = () => setRtStatus('error');

    es.addEventListener('message', (evt: MessageEvent) => {
      try {
        const m = JSON.parse(String((evt as any).data || '{}')) as any;
        const id = String(m.id || '');
        if (!id || knownMsgIdsRef.current.has(id)) return;
        knownMsgIdsRef.current.add(id);

        const attachments = Array.isArray(m.attachments) ? m.attachments : [];
        const imgRaw = attachments
          .filter((a: any) => String(a.kind || '').toLowerCase() === 'image')
          .map((a: any) => ({
            id: a.id,
            mime_type: a.mime_type || a.mimeType,
            url: a.url,
            file_name: a.file_name || a.fileName,
            size: a.size,
          }));
        const fileRaw = attachments
          .filter((a: any) => String(a.kind || '').toLowerCase() === 'file')
          .map((a: any) => ({
            id: a.id,
            url: a.url,
            file_name: a.file_name || a.fileName,
            size: a.size,
            mime_type: a.mime_type || a.mimeType,
          }));

        const next: ChatMsg = {
          id,
          role: (m.role as any) === 'assistant' ? 'assistant' : 'user',
          content: String(m.content || ''),
          format: 'markdown',
          runId: m.run_id,
          userMessageId: m.user_message_id,
          timestamp: m.timestamp,
          images: normalizeImageAttachments(imgRaw, 'history'),
          files: normalizeFileAttachments(fileRaw),
        };

        // If a slash command is pending, route the first assistant reply to the panel.
        const pending = pendingCmdRef.current;
        if (pending && next.role === 'assistant') {
          pendingCmdRef.current = null;
          setCmdResult({ command: pending, content: next.content, format: 'markdown' });
          setTyping(false);
          return;
        }

        setMessages((prev) => [...prev, next]);

        // Best-effort: when a message arrives, refresh run_events so auto/expanded modes can update.
        if (runTraceMode !== 'hidden') {
          getSession(projectName, currentSessionId, 1, 200).then((detail) => {
            setRunEvents(normalizeRunEvents(detail.run_events));
          }).catch(() => {});
        }

        if (next.role === 'assistant') setTyping(false);
      } catch {
        // ignore parse errors
      }
    });

    es.addEventListener('run_event', (evt: MessageEvent) => {
      try {
        const e = JSON.parse(String((evt as any).data || '{}')) as RunEvent;
        if (!e?.id) return;
        setRunEvents((prev) => mergeRunEvent(prev, e));
      } catch {
        // ignore parse errors
      }
    });

    return () => {
      es.close();
      setRtStatus('disconnected');
    };
  }, [projectName, currentSessionId, runTraceMode]);

  const ensureCurrentSession = useCallback(async () => {
    if (currentSession) return currentSession;
    if (!projectName || !sessionKey) return null;

    const created = await createSession(projectName, { session_key: sessionKey });
    if (!created?.id) return null;

    const detail = await getSession(projectName, created.id, 200);
    setUserPickedSession(true);
    applySessionDetail(detail);
    setSessions(prev => [detail, ...prev.filter((item) => item.id !== detail.id)]);
    return detail;
  }, [currentSession, projectName, sessionKey, applySessionDetail]);

  const handleNewSession = useCallback(async () => {
    if (!projectName || !sessionKey) return;
    setDrawerOpen(false);
    setLoading(true);
    try {
      const created = await createSession(projectName, { session_key: sessionKey });
      if (!created?.id) {
        await fetchData();
        return;
      }
      const detail = await getSession(projectName, created.id, 200);
      setUserPickedSession(true);
      applySessionDetail(detail);
      setSessions(prev => [detail, ...prev.filter((item) => item.id !== detail.id)]);
      setTyping(false);
      pendingCmdRef.current = null;
    } finally {
      setLoading(false);
    }
  }, [projectName, sessionKey, fetchData, applySessionDetail]);

  // Scroll to bottom on new messages
  useEffect(() => {
    messagesEnd.current?.scrollIntoView({ behavior: 'smooth' });
  }, [messages, typing]);

  // Send message
  const handleSend = useCallback(async (content: string, images: WebImageAttachment[] = []) => {
    const hasImages = images.length > 0;
    if (!content.trim() && !hasImages) return false;
    setSending(true);
    try {
      if (!hasImages && content.trim() === '/new') {
        await handleNewSession();
        return true;
      }

      const targetSession = await ensureCurrentSession();
      if (!targetSession) return false;

      const cmdToken = content.split(' ')[0];
      const isKnownCmd = knownCommands.has(cmdToken);
      if (!hasImages && isKnownCmd && !chatCommands.has(cmdToken)) {
        pendingCmdRef.current = cmdToken;
      }
      await sendMessage(projectName || '', {
        session_key: sessionKey,
        session_id: targetSession.id,
        message: content,
        images: hasImages ? toBridgeImagePayload(images) : undefined,
      });
      setTyping(true);
      return true;
    } finally {
      setTimeout(() => setSending(false), 300);
    }
  }, [ensureCurrentSession, handleNewSession, projectName, sessionKey]);

  // Commands whose result should go to the message stream (they change state)
  const chatCommands = new Set(['/new', '/stop', '/switch', '/delete-mode', '/upgrade']);
  const knownCommands = new Set(slashCommands.map(c => c.cmd));

  const handleCmdSelect = useCallback(async (cmd: SlashCommand) => {
    if (cmd.cmd === '/new') {
      await handleNewSession();
      return;
    }
    if (chatCommands.has(cmd.cmd)) {
      // Let SSE deliver the durable user message.
    } else {
      pendingCmdRef.current = cmd.cmd;
    }
    const targetSession = await ensureCurrentSession();
    if (!targetSession) return;
    await sendMessage(projectName || '', { session_key: sessionKey, session_id: targetSession.id, message: cmd.cmd });
    setTyping(true);
  }, [ensureCurrentSession, handleNewSession, projectName, sessionKey]);

  const handleCardAction = useCallback((value: string) => {
    // If the command panel is showing, route the follow-up response back to it
    if (cmdPanelRef.current) {
      pendingCmdRef.current = cmdPanelRef.current;
    }
    ensureCurrentSession().then((targetSession) => {
      if (!targetSession) return;
      return sendMessage(projectName || '', {
        session_key: sessionKey,
        session_id: targetSession.id,
        action: value,
        reply_ctx: sessionKey,
      });
    }).then(() => {
      setTyping(true);
    }).catch(() => {});
  }, [ensureCurrentSession, projectName, sessionKey]);

  const handleRenameSession = useCallback(async (session: Session, name: string) => {
    if (!projectName) return;
    await updateSession(projectName, session.id, { name });
    setSessions(prev => prev.map((item) => (item.id === session.id ? { ...item, name } : item)));
    if (currentSession?.id === session.id) {
      setCurrentSession(prev => (prev ? { ...prev, name } : prev));
    }
  }, [projectName, currentSession?.id]);

  const runEventsByUserMsg = useMemo(() => {
    const map = new Map<string, RunEvent[]>();
    for (const e of runEvents) {
      const key = (e as any).user_message_id || (e as any).run_id;
      if (!key) continue;
      const arr = map.get(key) || [];
      arr.push(e);
      map.set(key, arr);
    }
    return map;
  }, [runEvents]);

  const activePendingUserMsgId = useMemo(() => {
    for (let i = messages.length - 1; i >= 0; i--) {
      if (messages[i].role !== 'user') continue;
      return hasAssistantFinalAfterUser(messages, i) ? '' : messages[i].id;
    }
    return '';
  }, [messages]);

  // Poll run_events while a run is active so "auto/expanded/collapsed" can update.
  useEffect(() => {
    if (!projectName || !currentSessionId) return;
    if (runTraceMode === 'hidden') return;
    if (!activePendingUserMsgId) return;

    const timer = setInterval(() => {
      getSession(projectName, currentSessionId, 1, 200)
        .then((detail) => setRunEvents(normalizeRunEvents(detail.run_events)))
        .catch(() => {});
    }, 1500);
    return () => clearInterval(timer);
  }, [projectName, currentSessionId, runTraceMode, activePendingUserMsgId]);

  // Sending is REST-based and does not require SSE to be connected; SSE is best-effort for live updates.
  const canSend = Boolean(projectName && sessionKey);

  if (loading && !currentSession && sessions.length === 0) {
    return <div className="flex items-center justify-center h-64 text-gray-400 animate-pulse">Loading...</div>;
  }

  return (
    <div className="flex flex-col h-[calc(100dvh-8rem)] sm:h-[calc(100dvh-8rem)] animate-fade-in min-w-0">
      {/* Header */}
      <div className="flex items-start sm:items-center justify-between gap-3 pb-3 border-b border-gray-200 dark:border-gray-800 shrink-0">
        <div className="flex items-start gap-2 sm:gap-3 min-w-0">
          <Link to="/chat" className="p-2 min-h-10 min-w-10 rounded-lg hover:bg-gray-100 dark:hover:bg-gray-800 transition-colors shrink-0">
            <ArrowLeft size={18} className="text-gray-400" />
          </Link>
          <div className="min-w-0">
            <div className="flex items-center gap-2 flex-wrap">
              <h2 className="text-base sm:text-lg font-semibold text-gray-900 dark:text-white truncate max-w-[60vw] sm:max-w-none">{projectName}</h2>
              <StatusBadge status={rtStatus} />
              <select
                value={activeFrontendSlotName}
                onChange={(e) => handleWebPlatformChange(e.target.value)}
                className="h-8 rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 px-2 text-xs text-gray-600 dark:text-gray-300 focus:outline-none focus:ring-2 focus:ring-accent/40 max-w-[45vw] sm:max-w-none"
                title="Frontend slot"
              >
                {frontendSlotOptions.map((slot) => (
                  <option key={slot.slot} value={slot.slot}>
                    {slot.label ? `${slot.label} (${slot.slot})` : slot.slot}
                  </option>
                ))}
              </select>
            </div>
            <button
              type="button"
              onClick={() => setDrawerOpen(true)}
              className="flex items-center gap-1 text-xs text-gray-500 hover:text-accent transition-colors mt-0.5"
            >
              <span>{userPickedSession && currentSession
                ? (currentSession.name || currentSession.id.slice(0, 8))
                : t('chat.defaultSession')}</span>
              <ChevronDown size={12} />
            </button>
          </div>
        </div>
      </div>

      {/* Messages */}
      <div className="flex-1 overflow-y-auto py-4 sm:py-6 space-y-4 sm:space-y-5 min-w-0">
        {messages.length === 0 && !loading && (
          <div className="flex flex-col items-center justify-center h-full text-center py-12">
            <div className="w-16 h-16 rounded-2xl bg-accent/10 flex items-center justify-center mb-4">
              <Bot size={32} className="text-accent" />
            </div>
            <p className="text-sm text-gray-500 dark:text-gray-400 mb-1">{t('chat.emptyHint')}</p>
            <p className="text-xs text-gray-400 dark:text-gray-500">{t('chat.slashHint')}</p>
          </div>
        )}
        {messages.map((msg, idx) => {
          const isUser = msg.role === 'user';
          const events = isUser ? (runEventsByUserMsg.get(msg.id) || []) : [];
          const hasFinal = isUser ? hasAssistantFinalAfterUser(messages, idx) : false;
          const shouldShowRunTrace =
            isUser &&
            events.length > 0 &&
            (runTraceMode === 'expanded' ||
              runTraceMode === 'collapsed' ||
              (runTraceMode === 'auto' && !hasFinal));

          return (
            <Fragment key={msg.id}>
              <MessageBubble msg={msg} onAction={handleCardAction} />
              {shouldShowRunTrace && (
                <RunTrace
                  mode={runTraceMode}
                  events={events}
                  open={Boolean(openRunTraces[msg.id])}
                  onToggle={runTraceMode === 'collapsed'
                    ? () => setOpenRunTraces((prev) => ({ ...prev, [msg.id]: !prev[msg.id] }))
                    : undefined}
                />
              )}
            </Fragment>
          );
        })}
        {typing && !messages.some(m => m.streaming) && (
          <TypingIndicator />
        )}
        <div ref={messagesEnd} />
      </div>

      {/* Input area */}
      <div className="border-t border-gray-200 dark:border-gray-800 pt-3 shrink-0 safe-bottom">
        {canSend ? (
          <ChatComposer
            canSend={canSend}
            sending={sending}
            placeholder={t('chat.inputPlaceholder')}
            commandLabel={t('chat.commands')}
            onSend={handleSend}
            onCommandSelect={handleCmdSelect}
          />
        ) : rtStatus === 'disconnected' || rtStatus === 'error' ? (
          <div className="flex items-center gap-2 px-4 py-3 text-sm text-amber-600 dark:text-amber-400 bg-amber-50 dark:bg-amber-900/20 rounded-xl">
            <WifiOff size={14} />
            <span>{t('sessions.bridgeDisconnected')}</span>
          </div>
        ) : (
          <div className="flex items-center gap-2 px-4 py-3 text-sm text-gray-400 bg-gray-50 dark:bg-gray-800/50 rounded-xl">
            <Loader2 size={14} className="animate-spin" />
            <span>{t('sessions.bridgeConnecting')}</span>
          </div>
        )}
      </div>

      {/* Session drawer */}
      <SessionDrawer
        open={drawerOpen}
        onClose={() => setDrawerOpen(false)}
        sessions={sessions}
        currentSessionId={currentSession?.id || ''}
        onSelect={switchToSession}
        onRename={handleRenameSession}
        onNewSession={handleNewSession}
      />

      {/* Command result panel */}
      <CommandResultPanel
        result={cmdResult}
        onClose={() => setCmdResult(null)}
        onCardAction={handleCardAction}
      />
    </div>
  );
}
