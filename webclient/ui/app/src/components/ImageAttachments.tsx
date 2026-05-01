import { Download, X } from 'lucide-react';
import {
  bytesToLabel,
  downloadImageAttachment,
  imageAttachmentSrc,
  type WebImageAttachment,
} from '@/lib/attachments';
import { cn } from '@/lib/utils';

export function ImageAttachmentGrid({
  images,
  className,
  downloadLabel = 'Download image',
}: {
  images: WebImageAttachment[];
  className?: string;
  downloadLabel?: string;
}) {
  if (!images.length) return null;
  return (
    <div className={cn('grid grid-cols-2 gap-2 sm:max-w-md', images.length === 1 && 'grid-cols-1', className)}>
      {images.map((image) => {
        const src = imageAttachmentSrc(image);
        if (!src) return null;
        return (
          <figure key={image.id} className="group relative overflow-hidden rounded-xl border border-black/10 dark:border-white/10 bg-black/5 dark:bg-white/5">
            <a href={src} target="_blank" rel="noreferrer" className="block">
              <img
                src={src}
                alt={image.fileName || 'attached image'}
                className="h-36 w-full object-cover sm:h-44"
                loading="lazy"
              />
            </a>
            <button
              type="button"
              onClick={() => downloadImageAttachment(image)}
              className="absolute right-2 top-2 flex h-8 w-8 items-center justify-center rounded-lg bg-black/65 text-white opacity-100 shadow-sm transition hover:bg-black/80 sm:opacity-0 sm:group-hover:opacity-100"
              title={downloadLabel}
              aria-label={downloadLabel}
            >
              <Download size={15} />
            </button>
            {(image.fileName || image.size !== undefined) && (
              <figcaption className="absolute inset-x-0 bottom-0 bg-gradient-to-t from-black/70 to-transparent px-2 pb-2 pt-6 text-[11px] text-white">
                <div className="truncate">{image.fileName || 'image'}</div>
                {image.size !== undefined && <div className="text-white/70">{bytesToLabel(image.size)}</div>}
              </figcaption>
            )}
          </figure>
        );
      })}
    </div>
  );
}

export function ImageAttachmentPreview({
  images,
  onRemove,
  removeLabel = 'Remove image',
}: {
  images: WebImageAttachment[];
  onRemove: (id: string) => void;
  removeLabel?: string;
}) {
  if (!images.length) return null;
  return (
    <div className="flex gap-2 overflow-x-auto pb-1">
      {images.map((image) => {
        const src = imageAttachmentSrc(image);
        if (!src) return null;
        return (
          <div key={image.id} className="relative h-20 w-20 shrink-0 overflow-hidden rounded-xl border border-gray-200 bg-gray-100 dark:border-gray-700 dark:bg-gray-800">
            <img src={src} alt={image.fileName || 'selected image'} className="h-full w-full object-cover" />
            <button
              type="button"
              onClick={() => onRemove(image.id)}
              className="absolute right-1 top-1 flex h-6 w-6 items-center justify-center rounded-md bg-black/65 text-white transition hover:bg-black/80"
              title={removeLabel}
              aria-label={removeLabel}
            >
              <X size={13} />
            </button>
            {image.fileName && (
              <div className="absolute inset-x-0 bottom-0 truncate bg-black/55 px-1.5 py-0.5 text-[10px] text-white">
                {image.fileName}
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}
