import asyncio
from typing import Callable, Dict, List, Any
from .data_types import EventType

class Event:
    def __init__(self, type: EventType, data: Any = None):
        self.type = type
        self.data = data

class EventEngine:
    """
    轻量级异步事件引擎
    """
    def __init__(self):
        self._queue = asyncio.Queue()
        self._handlers: Dict[EventType, List[Callable]] = {}
        self._active = False
        self._task = None

    def start(self):
        """启动事件处理循环"""
        self._active = True
        self._task = asyncio.create_task(self._run())
        print("Event Engine Started")

    def stop(self):
        """停止事件处理循环"""
        self._active = False
        if self._task:
            self._task.cancel()

    def register(self, type: EventType, handler: Callable):
        """注册事件监听器"""
        if type not in self._handlers:
            self._handlers[type] = []
        self._handlers[type].append(handler)

    def put(self, event: Event):
        """推送事件到队列"""
        self._queue.put_nowait(event)

    async def _run(self):
        """事件处理主循环"""
        while self._active:
            try:
                event = await self._queue.get()
                if event.type in self._handlers:
                    for handler in self._handlers[event.type]:
                        if asyncio.iscoroutinefunction(handler):
                            await handler(event)
                        else:
                            handler(event)
                self._queue.task_done()
            except asyncio.CancelledError:
                break
            except Exception as e:
                print(f"Event Engine Error: {e}")
