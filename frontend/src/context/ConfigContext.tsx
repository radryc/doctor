import {
  createContext,
  useContext,
  useState,
  useEffect,
  type ReactNode,
} from "react";
import { api } from "../api/client";
import type { UIConfig } from "../types/api";

interface ConfigState {
  config: UIConfig;
  loaded: boolean;
}

const defaultConfig: UIConfig = {
  default_tenant: "default",
  guardian_url: "",
  default_partition: "",
};

const ConfigContext = createContext<ConfigState>({
  config: defaultConfig,
  loaded: false,
});

export function ConfigProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<ConfigState>({
    config: defaultConfig,
    loaded: false,
  });

  useEffect(() => {
    api
      .getConfig()
      .then((config) => setState({ config, loaded: true }))
      .catch(() => setState({ config: defaultConfig, loaded: true }));
  }, []);

  return (
    <ConfigContext.Provider value={state}>{children}</ConfigContext.Provider>
  );
}

export function useConfig(): ConfigState {
  return useContext(ConfigContext);
}
