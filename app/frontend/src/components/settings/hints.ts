import { createContext } from 'react'

export interface SettingsHints {
  restartRequired: string[]
  envDiffers: string[]
  applyErrors: Record<string, string>
}

export const SettingsHintsContext = createContext<SettingsHints>({
  restartRequired: [],
  envDiffers: [],
  applyErrors: {},
})
