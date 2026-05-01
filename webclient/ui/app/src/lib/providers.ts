import type { GlobalProvider, ProviderPreset } from '@/api/providers';

export const AGENT_LABELS: Record<string, string> = {
  claudecode: 'Claude Code',
  codex: 'Codex',
  gemini: 'Gemini CLI',
  cursor: 'Cursor',
  acp: 'ACP',
  iflow: 'iFlow',
  opencode: 'OpenCode',
  qoder: 'Qoder',
  kimi: 'Kimi',
};

export function getAgentLabel(agentType: string): string {
  return AGENT_LABELS[agentType] || agentType;
}

export function isProviderCompatible(agentType: string, provider: Pick<GlobalProvider, 'agent_types'>): boolean {
  return !provider.agent_types || provider.agent_types.length === 0 || provider.agent_types.includes(agentType);
}

export function getPresetAgentTypes(preset: ProviderPreset): string[] {
  return preset.agents && preset.agents.length > 0 ? preset.agents : ['claudecode'];
}

export function getPresetConfigForAgent(preset: ProviderPreset, agentType: string): { base_url: string; model: string } {
  return {
    base_url: preset.endpoints?.[agentType] || preset.base_url || '',
    model: preset.agent_models?.[agentType] || preset.models?.[0] || '',
  };
}

export function getPresetProviderName(preset: ProviderPreset, agentType: string): string {
  const agentTypes = getPresetAgentTypes(preset);
  return agentTypes.length > 1 ? `${preset.name}-${agentType}` : preset.name;
}
