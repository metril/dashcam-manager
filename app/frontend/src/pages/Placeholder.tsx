interface Props {
  title: string
}

export default function Placeholder({ title }: Props) {
  return (
    <div className="flex h-full min-h-svh flex-col items-center justify-center gap-2 text-neutral-600">
      <div className="text-sm font-medium text-neutral-500">{title}</div>
      <div className="text-xs">arrives in a later milestone</div>
    </div>
  )
}
