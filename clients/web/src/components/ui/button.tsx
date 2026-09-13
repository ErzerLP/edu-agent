import * as React from 'react'
import { Slot } from '@radix-ui/react-slot'
import { cva, type VariantProps } from 'class-variance-authority'
import { cn } from '@/lib/utils'

const buttonVariants = cva(
  'button inline-flex items-center justify-center gap-2 disabled:cursor-not-allowed',
  {
    variants: {
      variant: {
        default: 'button-primary',
        outline: 'button-outline',
        destructive: 'button-danger',
        ghost: 'button-ghost',
      },
    },
    defaultVariants: { variant: 'default' },
  },
)
export function Button({
  className,
  variant,
  asChild = false,
  type = 'button',
  ...props
}: React.ComponentProps<'button'> & VariantProps<typeof buttonVariants> & { asChild?: boolean }) {
  const Comp = asChild ? Slot : 'button'
  return <Comp type={type} className={cn(buttonVariants({ variant, className }))} {...props} />
}
