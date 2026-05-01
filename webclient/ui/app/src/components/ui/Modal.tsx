import { cn } from '@/lib/utils';
import { X } from 'lucide-react';
import type { ReactNode } from 'react';

interface ModalProps {
  open: boolean;
  onClose: () => void;
  title: string;
  children: ReactNode;
  className?: string;
}

export function Modal({ open, onClose, title, children, className }: ModalProps) {
  if (!open) return null;
  return (
    <div className="fixed inset-0 z-50 flex items-end sm:items-center justify-center p-2 sm:p-4">
      <div
        className="absolute inset-0 bg-black/50 backdrop-blur-sm transition-opacity"
        onClick={onClose}
        role="presentation"
      />
      <div
        className={cn(
          'relative w-full max-w-lg max-h-[calc(100dvh-1rem)] overflow-y-auto rounded-2xl p-4 sm:p-6 shadow-2xl animate-fade-in safe-bottom',
          'bg-white/95 backdrop-blur-xl border border-gray-200/90',
          'dark:bg-[rgba(0,0,0,0.88)] dark:border-white/[0.1] dark:shadow-black/50',
          className
        )}
      >
        <div className="flex items-center justify-between gap-3 mb-4">
          <h2 className="text-base sm:text-lg font-semibold text-gray-900 dark:text-white truncate">{title}</h2>
          <button
            type="button"
            onClick={onClose}
            className={cn(
              'p-2 min-h-10 min-w-10 rounded-lg transition-colors duration-200 shrink-0',
              'text-gray-400 hover:bg-gray-100/90 dark:hover:bg-white/[0.08]'
            )}
            aria-label="Close"
          >
            <X size={18} />
          </button>
        </div>
        {children}
      </div>
    </div>
  );
}
