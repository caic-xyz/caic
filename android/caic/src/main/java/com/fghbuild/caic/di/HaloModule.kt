// Hilt module providing Halo BLE dependencies as singletons.
package com.fghbuild.caic.di

import android.content.Context
import com.caic.halo.ble.HaloConnection
import dagger.Module
import dagger.Provides
import dagger.hilt.InstallIn
import dagger.hilt.android.qualifiers.ApplicationContext
import dagger.hilt.components.SingletonComponent
import javax.inject.Singleton

@Module
@InstallIn(SingletonComponent::class)
object HaloModule {
    @Provides
    @Singleton
    fun provideHaloConnection(@ApplicationContext context: Context): HaloConnection =
        HaloConnection(context)
}
