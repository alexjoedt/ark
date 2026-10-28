import {
  createContext,
  useContext,
  useEffect,
  useState,
  useRef,
  type ReactNode,
} from "react";
import { toast } from "sonner";
import { WS_RECONNECT_DELAY } from "../constants";
import type { WebSocketEvent } from "../types";
import { API_HOST } from "../api";

type EventCallback = (event: WebSocketEvent) => void;

interface WebSocketContextValue {
  isConnected: boolean;
  subscribe: (eventType: string, callback: EventCallback) => void;
  unsubscribe: (eventType: string, callback: EventCallback) => void;
}

const WebSocketContext = createContext<WebSocketContextValue | undefined>(
  undefined
);

interface WebSocketProviderProps {
  children: ReactNode;
  isAuthenticated: boolean;
}

export function WebSocketProvider({
  children,
  isAuthenticated,
}: WebSocketProviderProps) {
  const [isConnected, setIsConnected] = useState(false);
  const wsRef = useRef<WebSocket | null>(null);
  const listenersRef = useRef<Map<string, Set<EventCallback>>>(new Map());
  const reconnectTimeoutRef = useRef<number | null>(null);
  const reconnectDelayRef = useRef(WS_RECONNECT_DELAY);
  const shouldConnectRef = useRef(false);

  const subscribe = (eventType: string, callback: EventCallback) => {
    const listeners = listenersRef.current.get(eventType) || new Set();
    listeners.add(callback);
    listenersRef.current.set(eventType, listeners);
  };

  const unsubscribe = (eventType: string, callback: EventCallback) => {
    const listeners = listenersRef.current.get(eventType);
    if (listeners) {
      listeners.delete(callback);
      if (listeners.size === 0) {
        listenersRef.current.delete(eventType);
      }
    }
  };

  const dispatch = (event: WebSocketEvent) => {
    // Dispatch to specific event type listeners
    const listeners = listenersRef.current.get(event.type);
    if (listeners) {
      listeners.forEach((callback) => callback(event));
    }

    // Dispatch to wildcard listeners
    const wildcardListeners = listenersRef.current.get("*");
    if (wildcardListeners) {
      wildcardListeners.forEach((callback) => callback(event));
    }
  };

  const connect = () => {
    if (!isAuthenticated || wsRef.current?.readyState === WebSocket.OPEN) {
      return;
    }

    // Clear any pending reconnection attempts
    if (reconnectTimeoutRef.current) {
      clearTimeout(reconnectTimeoutRef.current);
      reconnectTimeoutRef.current = null;
    }

    const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
    var host = window.location.host;
    try {
      var apiUrl = new URL(API_HOST);
      host = apiUrl.host;
    } catch {
      // nothing
      host = window.location.host;
    }

    const wsUrl = `${protocol}//${host}/ws`;

    try {
      const ws = new WebSocket(wsUrl);

      ws.onopen = () => {
        setIsConnected(true);
        reconnectDelayRef.current = WS_RECONNECT_DELAY;
      };

      ws.onmessage = (event) => {
        try {
          const data = JSON.parse(event.data) as WebSocketEvent;
          dispatch(data);

          // Show toasts for completed/failed operations
          if (data.type === "backup.completed" && "data" in data) {
            toast.success(`Backup completed: ${data.data.volume}`, {
            description: `Snapshot: ${data.data.snapshot_id}`,
            });
          } else if (data.type === "backup.failed" && "data" in data) {
            toast.error(`Backup failed: ${data.data.volume}`, {
              description: data.data.error,
            });
          } else if (data.type === "restore.completed" && "data" in data) {
            toast.success(`Restore completed: ${data.data.volume}`, {
              description: `Snapshot: ${data.data.snapshot_id}`,
            });
          } else if (data.type === "restore.failed" && "data" in data) {
            toast.error(`Restore failed: ${data.data.volume}`, {
              description: data.data.error,
            });
          }
        } catch (error) {
          console.error("Failed to parse WebSocket message:", error);
        }
      };

      ws.onerror = (error) => {
        console.error("WebSocket error:", error);
      };

      ws.onclose = () => {
        setIsConnected(false);
        wsRef.current = null;

        // Only attempt reconnection if we should still be connected
        if (shouldConnectRef.current && isAuthenticated) {
          const delay = Math.min(reconnectDelayRef.current, 30000);
          reconnectTimeoutRef.current = setTimeout(() => {
            reconnectDelayRef.current = delay * 2;
            connect();
          }, delay);
        }
      };

      wsRef.current = ws;
    } catch (error) {
      console.error("Failed to create WebSocket:", error);
    }
  };

  useEffect(() => {
    if (isAuthenticated) {
      shouldConnectRef.current = true;
      connect();
    } else {
      // Disconnect when not authenticated
      shouldConnectRef.current = false;
      if (wsRef.current) {
        wsRef.current.close();
        wsRef.current = null;
      }
      if (reconnectTimeoutRef.current) {
        clearTimeout(reconnectTimeoutRef.current);
        reconnectTimeoutRef.current = null;
      }
      setIsConnected(false);
    }

    return () => {
      // Mark that we should no longer connect
      shouldConnectRef.current = false;
      if (wsRef.current) {
        wsRef.current.close();
      }
      if (reconnectTimeoutRef.current) {
        clearTimeout(reconnectTimeoutRef.current);
      }
    };
  }, [isAuthenticated]);

  const value: WebSocketContextValue = {
    isConnected,
    subscribe,
    unsubscribe,
  };

  return (
    <WebSocketContext.Provider value={value}>
      {children}
    </WebSocketContext.Provider>
  );
}

export function useWebSocket(): WebSocketContextValue {
  const context = useContext(WebSocketContext);
  if (!context) {
    throw new Error("useWebSocket must be used within WebSocketProvider");
  }
  return context;
}
